package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Provisioning tests run against a local test server rather than Hugging Face.
//
// Deliberately: the properties worth testing are resumption, verification and atomic
// publication, and none of them need the real endpoint — while depending on it would
// make the suite fail on an offline machine and download 133 MB when it didn't.

// fakeModelServer serves a body with ranged-request support, and counts how many bytes
// it was asked for so a resume can be proven rather than assumed.
type fakeModelServer struct {
	body []byte
	// served counts bytes written across all requests.
	served int
	// requests records the Range header of each request ("" when absent).
	requests []string
	// cut, when > 0, closes the connection after that many bytes of the *first*
	// response, simulating an interrupted download.
	cut int
	n   int
}

func (f *fakeModelServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.n++
		rng := r.Header.Get("Range")
		f.requests = append(f.requests, rng)

		start := 0
		if rng != "" {
			// Only the "bytes=N-" form is produced by fetchFile.
			if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-")); err == nil {
				start = n
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d",
				start, len(f.body)-1, len(f.body)))
			w.WriteHeader(http.StatusPartialContent)
		}

		chunk := f.body[start:]
		if f.cut > 0 && f.n == 1 && len(chunk) > f.cut {
			chunk = chunk[:f.cut]
		}
		n, _ := w.Write(chunk)
		f.served += n
		if f.cut > 0 && f.n == 1 {
			// Force the client to see a short read by panicking the handler, which
			// closes the connection mid-body — the shape a real interruption takes.
			panic(http.ErrAbortHandler)
		}
	}
}

// TestProvisionVerifiesAndPublishesAtomically covers the two properties that make a
// download trustworthy: nothing is published until it verifies, and what lands is what
// was expected.
func TestProvisionVerifiesAndPublishesAtomically(t *testing.T) {
	body := []byte(strings.Repeat("model-bytes-", 5000))
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	srv := &fakeModelServer{body: body}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "model.onnx")

	if err := fetchFileFrom(context.Background(), ts.URL, dst, int64(len(body)), digest, nil); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Error("downloaded content does not match")
	}
	// No .part left behind: FindModel decides the model is present from a stat, so a
	// stray partial with the real name would be worse than no file at all.
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Error(".part file survived a successful download")
	}
}

// TestProvisionRejectsWrongDigest is the property that makes pinning the revision
// meaningful: a file of the right *size* but wrong content must not be installed.
func TestProvisionRejectsWrongDigest(t *testing.T) {
	body := []byte(strings.Repeat("x", 4096))
	srv := &fakeModelServer{body: body}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "model.onnx")
	wrong := strings.Repeat("a", 64)

	err := fetchFileFrom(context.Background(), ts.URL, dst, int64(len(body)), wrong, nil)
	if err == nil {
		t.Fatal("a file failing its digest was accepted")
	}
	if !strings.Contains(err.Error(), "failed verification") {
		t.Errorf("error should name the verification failure, got: %v", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Error("an unverified file was published")
	}
	// The rejected partial must be gone too: resuming from bytes known to be wrong
	// would fail forever.
	if _, statErr := os.Stat(dst + ".part"); !os.IsNotExist(statErr) {
		t.Error("the unverified .part was kept, so a retry would resume from bad bytes")
	}
}

// TestProvisionRejectsWrongSize covers the cheap check, which is the only one available
// for files with no published digest.
func TestProvisionRejectsWrongSize(t *testing.T) {
	body := []byte("short")
	srv := &fakeModelServer{body: body}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dst := filepath.Join(t.TempDir(), "tokenizer.json")
	err := fetchFileFrom(context.Background(), ts.URL, dst, 999999, "", nil)
	if err == nil {
		t.Fatal("a short file was accepted")
	}
	if !strings.Contains(err.Error(), "expected") {
		t.Errorf("error should state the expected size, got: %v", err)
	}
}

// TestProvisionResumes is the property that matters most on a 133 MB download over a
// bad link: a second attempt continues rather than starting over.
func TestProvisionResumes(t *testing.T) {
	body := []byte(strings.Repeat("resume-me-", 20000)) // 200 KB
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	srv := &fakeModelServer{body: body, cut: 50000}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dst := filepath.Join(t.TempDir(), "model.onnx")

	// First attempt is interrupted mid-body.
	if err := fetchFileFrom(context.Background(), ts.URL, dst, int64(len(body)), digest, nil); err == nil {
		t.Fatal("expected the interrupted download to fail")
	}
	part, err := os.Stat(dst + ".part")
	if err != nil {
		t.Fatalf("no partial file kept, so nothing could resume: %v", err)
	}
	if part.Size() == 0 || part.Size() >= int64(len(body)) {
		t.Fatalf("partial is %d bytes of %d — not a usable resume point", part.Size(), len(body))
	}

	// Second attempt must ask for a range and complete.
	var progress []ProvisionProgress
	if err := fetchFileFrom(context.Background(), ts.URL, dst, int64(len(body)), digest,
		func(p ProvisionProgress) { progress = append(progress, p) }); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(srv.requests) < 2 || srv.requests[1] == "" {
		t.Errorf("the second attempt sent no Range header: %v", srv.requests)
	}
	if srv.served >= 2*len(body) {
		t.Errorf("served %d bytes for a %d-byte file — it restarted rather than resumed",
			srv.served, len(body))
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Error("the resumed file is corrupt — the partial and the remainder did not join up")
	}
	var sawResumed bool
	for _, p := range progress {
		if p.Resumed {
			sawResumed = true
		}
	}
	if !sawResumed {
		t.Error("progress never reported the download as resumed")
	}
}

// TestProvisionSkipsVerifiedExistingFile keeps the UI button idempotent: pressing it
// with a good model already on disk must not re-download 133 MB.
func TestProvisionSkipsVerifiedExistingFile(t *testing.T) {
	body := []byte("already here")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	srv := &fakeModelServer{body: body}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dst := filepath.Join(t.TempDir(), "model.onnx")
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fetchFileFrom(context.Background(), ts.URL, dst, int64(len(body)), digest, nil); err != nil {
		t.Fatal(err)
	}
	if srv.n != 0 {
		t.Errorf("made %d request(s) for a file already present and verified", srv.n)
	}
}

// TestProvisionReplacesCorruptExistingFile is the other half: a file of the right size
// whose contents are wrong must be re-fetched, not trusted.
func TestProvisionReplacesCorruptExistingFile(t *testing.T) {
	body := []byte("the real content")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	srv := &fakeModelServer{body: body}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dst := filepath.Join(t.TempDir(), "model.onnx")
	// Same length, different bytes — exactly what a size-only check would miss.
	if err := os.WriteFile(dst, []byte("the FAKE content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fetchFileFrom(context.Background(), ts.URL, dst, int64(len(body)), digest, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != string(body) {
		t.Errorf("a same-size corrupt file was trusted: %q", got)
	}
}

// TestModelTargetDirIsUserCache pins the destination, which matters because an
// installed application directory is often read-only.
func TestModelTargetDirIsUserCache(t *testing.T) {
	dir, err := ModelTargetDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	if !strings.Contains(filepath.ToSlash(dir), ".cache/localchat/models/"+ModelDirName) {
		t.Errorf("target %q is not the user cache", dir)
	}
	// It must be one of the places FindModel looks, or a download would succeed and
	// the model would still read as absent.
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".cache", "localchat", "models", ModelDirName)
	if dir != want {
		t.Errorf("target %q does not match FindModel's cache location %q", dir, want)
	}
}
