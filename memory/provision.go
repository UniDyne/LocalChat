package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Model provisioning (§3.1a).
//
// The model is fetched on demand rather than bundled: the installer stays lean, and
// memory is opt-in anyway, so a 133 MB download that most of the binary's users never
// need does not belong in it. Five properties matter, and each one is a decision:
//
//  1. **Never automatic.** An unannounced outbound request to huggingface.co on first
//     launch would be surprising in a local-first app. This runs only when called.
//  2. **Pinned revision, not `main`.** If BAAI re-exports model.onnx, `main` silently
//     yields a *different* model under the same name — and every vector already stored
//     was produced by the old one. Mixed vector spaces, no error, quietly worse recall.
//     Pinning is the same class of protection as the memory_meta model check, upstream.
//  3. **Verified against a known digest.** The size and SHA-256 are compiled in, so a
//     truncated, substituted or corrupted file is rejected rather than loaded. Without
//     this the pinning buys nothing: it is what makes "the model we intended" checkable.
//  4. **Resumable.** The endpoint serves ranged requests, so an interrupted 133 MB
//     download continues instead of restarting.
//  5. **Atomic.** Download to a temp file, verify, *then* rename into place, so a
//     partial file is never visible to FindModel — which decides the model is present
//     from a stat alone.

// Provisioning constants. The revision and digest live in embed.go beside the model's
// identity; these are the transport details.
const (
	// modelBaseURL is the pinned-revision download root. Interpolating ModelRevision
	// rather than "main" is the point — see property 2 above.
	modelBaseURL = "https://huggingface.co/BAAI/bge-small-en-v1.5/resolve/"
	// TokenizerBytes is tokenizer.json's expected size. It has no published LFS digest
	// (it is not an LFS object), so size is the only cheap integrity check available —
	// stated rather than hidden, since it is weaker than the model's SHA-256.
	TokenizerBytes = 711396
	// provisionTimeout bounds the whole download. Generous: 133 MB on a slow link.
	provisionTimeout = 30 * time.Minute
)

// ProvisionProgress reports download progress.
type ProvisionProgress struct {
	File      string  `json:"file"`
	Done      int64   `json:"done"`
	Total     int64   `json:"total"`
	Percent   float64 `json:"percent"`
	Resumed   bool    `json:"resumed"`
	Verifying bool    `json:"verifying"`
	Complete  bool    `json:"complete"`
	Err       string  `json:"error,omitempty"`
}

// ModelTargetDir is where a fetched model is written: the user cache, not beside the
// binary, because an installed application directory is often read-only.
func ModelTargetDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".cache", "localchat", "models", ModelDirName), nil
}

// ProvisionModel downloads and verifies the embedding model.
//
// Idempotent: if a verified model is already present it returns immediately, so calling
// this from a UI button is safe regardless of state. onProgress may be nil.
func ProvisionModel(ctx context.Context, onProgress func(ProvisionProgress)) (ModelPaths, error) {
	report := func(p ProvisionProgress) {
		if onProgress != nil {
			onProgress(p)
		}
	}

	if m, err := FindModel(); err == nil {
		report(ProvisionProgress{File: "model.onnx", Complete: true, Percent: 100})
		return m, nil
	}

	dir, err := ModelTargetDir()
	if err != nil {
		return ModelPaths{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ModelPaths{}, fmt.Errorf("create model directory %s: %w", dir, err)
	}

	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()

	// tokenizer.json first: it is 0.7 MB against the model's 133 MB, so if something is
	// wrong with the network, the endpoint or the pinned revision, it fails in a second
	// instead of after a long download.
	if err := fetchFile(ctx, "tokenizer.json", filepath.Join(dir, "tokenizer.json"),
		TokenizerBytes, "", report); err != nil {
		return ModelPaths{}, err
	}
	if err := fetchFile(ctx, "onnx/model.onnx", filepath.Join(dir, "model.onnx"),
		ModelBytes, ModelDigest, report); err != nil {
		return ModelPaths{}, err
	}

	m, err := FindModel()
	if err != nil {
		return ModelPaths{}, fmt.Errorf("model downloaded but not found afterwards: %w", err)
	}
	report(ProvisionProgress{File: "model.onnx", Complete: true, Percent: 100,
		Done: ModelBytes, Total: ModelBytes})
	slog.Info("embedding model provisioned", "dir", dir, "revision", ModelRevision)
	return m, nil
}

// fetchFile downloads one file to dst, resuming a partial `.part` if present, then
// verifies size and (when given) SHA-256 before renaming into place.
//
// wantDigest may be empty for files with no published digest; size is then the only
// check, which is weaker and is reported as such by the caller's constant comment.
func fetchFile(ctx context.Context, remote, dst string, wantSize int64, wantDigest string,
	report func(ProvisionProgress),
) error {
	return fetchFileFrom(ctx, modelBaseURL+ModelRevision+"/"+remote, dst, wantSize, wantDigest, report)
}

// fetchFileFrom is fetchFile against an explicit URL, so the resume/verify/publish logic
// can be tested against a local server instead of Hugging Face — the properties worth
// testing are those three, and depending on the real endpoint would both fail offline
// and download 133 MB to prove it.
func fetchFileFrom(ctx context.Context, url, dst string, wantSize int64, wantDigest string,
	report func(ProvisionProgress),
) error {
	if report == nil {
		report = func(ProvisionProgress) {}
	}
	name := filepath.Base(dst)
	if fi, err := os.Stat(dst); err == nil && fi.Size() == wantSize {
		// Already there and the right size. Re-verify the digest rather than trusting
		// the size, since a substituted file of equal length is exactly what the digest
		// exists to catch.
		if wantDigest == "" {
			return nil
		}
		if sum, err := fileDigest(dst); err == nil && sum == wantDigest {
			return nil
		}
		slog.Warn("existing file failed verification; re-downloading", "file", dst)
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("remove unverified %s: %w", dst, err)
		}
	}

	part := dst + ".part"
	var resumeAt int64
	if fi, err := os.Stat(part); err == nil {
		resumeAt = fi.Size()
		if resumeAt > wantSize {
			// A partial larger than the target cannot be a prefix of it. Start over
			// rather than trying to reason about how it got that way.
			resumeAt = 0
			_ = os.Remove(part)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if resumeAt > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(resumeAt, 10)+"-")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// The server ignored the range (or we asked for none): start from zero.
		resumeAt = 0
	case http.StatusPartialContent:
		// Resuming as requested.
	default:
		return fmt.Errorf("download %s: unexpected status %s", name, resp.Status)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if resumeAt > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", part, err)
	}

	done := resumeAt
	last := time.Now()
	report(ProvisionProgress{File: name, Done: done, Total: wantSize,
		Percent: pct(done, wantSize), Resumed: resumeAt > 0})

	buf := make([]byte, 1<<20)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", part, werr)
			}
			done += int64(n)
			// Throttled so a UI event per megabyte does not become one per read.
			if time.Since(last) > 250*time.Millisecond {
				last = time.Now()
				report(ProvisionProgress{File: name, Done: done, Total: wantSize,
					Percent: pct(done, wantSize), Resumed: resumeAt > 0})
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			f.Close()
			// The partial is deliberately left in place: that is what makes the next
			// attempt a resume rather than a restart.
			return fmt.Errorf("download %s: %w", name, readErr)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", part, err)
	}

	// Verify before publishing. A file that fails here is removed, because leaving it
	// as a `.part` would make the next attempt resume from bytes known to be wrong.
	report(ProvisionProgress{File: name, Done: done, Total: wantSize, Percent: 100, Verifying: true})
	if fi, err := os.Stat(part); err != nil {
		return err
	} else if fi.Size() != wantSize {
		_ = os.Remove(part)
		return fmt.Errorf("%s is %d bytes, expected %d — the pinned revision may have "+
			"changed, or the download was corrupted", name, fi.Size(), wantSize)
	}
	if wantDigest != "" {
		sum, err := fileDigest(part)
		if err != nil {
			return err
		}
		if sum != wantDigest {
			_ = os.Remove(part)
			return fmt.Errorf("%s failed verification: sha256 %s, expected %s. The file "+
				"at the pinned revision is not the one this build expects; do not use it",
				name, sum, wantDigest)
		}
	}

	// Atomic publish, so FindModel never sees a partial file as a present one.
	if err := os.Rename(part, dst); err != nil {
		return fmt.Errorf("install %s: %w", dst, err)
	}
	return nil
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func pct(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return 100 * float64(done) / float64(total)
}
