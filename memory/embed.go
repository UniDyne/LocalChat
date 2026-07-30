package memory

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// EmbedDims is the embedding width the schema is built for. store.EmbedDim must
// agree; TestSchemaDimensionMatchesConstant asserts it.
const EmbedDims = 384

// QueryPrefix is what BGE v1.5 expects on queries. Passages are embedded bare.
//
// The asymmetry lives here and nowhere else: applying the prefix to both, or to
// neither, measurably degrades recall, and Phase 0 confirmed it changes the
// vector materially (cosine 0.943 between prefixed and unprefixed).
const QueryPrefix = "Represent this sentence for searching relevant passages: "

// Embedder turns text into unit-length vectors.
//
// Passages and queries are separate methods rather than one method plus a flag,
// so a caller cannot accidentally embed a query as a passage — the failure that
// would produce would be silent.
type Embedder interface {
	// EmbedPassages embeds stored content. Vectors are L2-normalized.
	EmbedPassages(ctx context.Context, texts []string) ([][]float32, error)
	// EmbedQuery embeds a search query, applying the model's query prefix.
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	// Dims is the vector width.
	Dims() int
	// ModelName identifies the model, recorded in memory_meta so a change is
	// detected rather than silently mixing vector spaces.
	ModelName() string
	// Close releases resources.
	Close() error
}

// ErrUnavailable reports that embeddings cannot be produced — the ONNX Runtime
// library or the model file is missing, or the two are incompatible.
//
// This is a first-class outcome, not an error path to crash on: memory is opt-in,
// and an unprovisioned model must leave the app fully usable while saying exactly
// what is wrong. Retrieval degrades to the three non-vector signals.
type ErrUnavailable struct {
	Reason string
	Err    error
}

func (e *ErrUnavailable) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("memory unavailable: %s: %v", e.Reason, e.Err)
	}
	return "memory unavailable: " + e.Reason
}

func (e *ErrUnavailable) Unwrap() error { return e.Err }

// Unavailable reports whether err indicates embeddings are unavailable.
func Unavailable(err error) bool {
	var u *ErrUnavailable
	return errors.As(err, &u)
}

// ---------- model and library resolution ----------

// ONNX Runtime version floor, set by the pinned binding rather than by us.
//
// onnxruntime_go compiles ORT_API_VERSION in from its bundled header and offers no way
// to request less, so the binding chooses the floor. These constants exist so the error
// message can name it: a user told "requires ORT 1.21 or newer" can act, whereas
// "initialization failed" sends them reading source.
//
// Keep them in step with the pinned binding — read the number out of the module's
// onnxruntime_c_api.h, because the binding's own version does not track it (v1.19.0
// requests 21, v1.20.0 requests 22).
const (
	RequiredORTAPIVersion = 21
	MinORTVersion         = "1.21"
)

// ModelDirName is the pinned model's directory name. The revision and digest are
// recorded so a silently re-exported model is caught (§3.1a).
const (
	ModelDirName  = "bge-small-en-v1.5"
	ModelRevision = "5c38ec7c405ec4b44b94cc5a9bb96e735b38267a"
	ModelDigest   = "828e1496d7fabb79cfa4dcd84fa38625c0d3d21da474a00f08db0f559940cf35"
	ModelBytes    = 133093490
)

// ModelPaths describes a located model.
type ModelPaths struct {
	Dir           string
	ModelONNX     string
	TokenizerJSON string
}

// FindModel locates the model directory, in the resolution order from §3.1a:
// explicit config, the user data dir cache, then alongside the binary.
func FindModel() (ModelPaths, error) {
	var tried []string
	check := func(dir string) (ModelPaths, bool) {
		m := ModelPaths{
			Dir:           dir,
			ModelONNX:     filepath.Join(dir, "model.onnx"),
			TokenizerJSON: filepath.Join(dir, "tokenizer.json"),
		}
		tried = append(tried, dir)
		if _, err := os.Stat(m.ModelONNX); err != nil {
			return ModelPaths{}, false
		}
		if _, err := os.Stat(m.TokenizerJSON); err != nil {
			return ModelPaths{}, false
		}
		return m, true
	}

	if d := os.Getenv("LOCALCHAT_MODEL_DIR"); d != "" {
		if m, ok := check(d); ok {
			return m, nil
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if m, ok := check(filepath.Join(home, ".cache", "localchat", "models", ModelDirName)); ok {
			return m, nil
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if m, ok := check(filepath.Join(dir, "models", ModelDirName)); ok {
			return m, nil
		}
	}
	return ModelPaths{}, &ErrUnavailable{
		Reason: fmt.Sprintf("embedding model not provisioned (looked in: %s)", strings.Join(tried, ", ")),
	}
}

// FindORT locates libonnxruntime.
//
// A system package may install only versioned sonames — this box has
// libonnxruntime.so.1.21 and .so.1.21.0 but no unversioned libonnxruntime.so.1 — so
// versioned names must be globbed and the highest chosen.
//
// **On the version floor, which moved.** The binding compiles in a required
// ORT_API_VERSION from its bundled header and cannot be asked for less, so the binding
// sets a floor on the library. Phase 0 used onnxruntime_go v1.31.0, which requests API
// 26 and therefore rejected Debian's ORT 1.21 outright. That made shipping our own
// library mandatory and the system path nearly useless.
//
// The binding is now pinned to **v1.19.0, which requests API 21** — the newest binding
// ORT 1.21.0 accepts, so Debian 13's `libonnxruntime1.21` package works as-is. Note the
// binding's own version number does not track the API it requests: v1.19.0 and v1.20.0
// differ by one release but request 21 and 22 respectively, so this has to be read out
// of the bundled header rather than inferred.
//
// Compatibility runs one way and pinning low exploits it: a *newer* library serves an
// *older* requested API, so API 21 also works against 1.28 and later. Verified — the two
// produce vectors with cosine 1.0000000000 and a max component delta of 9e-8, i.e. the
// same vector space, so a corpus embedded under one runtime stays valid under the other.
func FindORT() (path string, how string, err error) {
	exeDir := ""
	if exe, e := os.Executable(); e == nil {
		exeDir = filepath.Dir(exe)
	}
	home, _ := os.UserHomeDir()
	return resolveORT(os.Getenv("ONNXRUNTIME_LIB"), exeDir, home, systemLibDirs())
}

// resolveORT is FindORT's logic with its inputs injected, so the not-found path
// can be tested without mutating process environment — which matters because ORT
// initialization is a process-global sync.Once and env mutation from one test
// would otherwise poison every later one.
func resolveORT(envPath, exeDir, homeDir string, sysDirs []string) (path string, how string, err error) {
	var tried []string

	if envPath != "" {
		tried = append(tried, "$ONNXRUNTIME_LIB="+envPath)
		if _, err := os.Stat(envPath); err == nil {
			return envPath, "ONNXRUNTIME_LIB", nil
		}
	}

	if exeDir != "" {
		for _, cand := range bundledORTNames(exeDir) {
			tried = append(tried, cand)
			if _, err := os.Stat(cand); err == nil {
				return cand, "bundled alongside binary", nil
			}
		}
	}

	// User cache, mirroring where the model is provisioned. This is what makes a
	// development machine work without setting ONNXRUNTIME_LIB, and gives the UI
	// somewhere to place a downloaded runtime.
	if homeDir != "" {
		for _, n := range []string{"libonnxruntime.so", "libonnxruntime.dylib", "onnxruntime.dll"} {
			cand := filepath.Join(homeDir, ".cache", "localchat", "lib", n)
			tried = append(tried, cand)
			if _, err := os.Stat(cand); err == nil {
				return cand, "user cache", nil
			}
		}
	}

	for _, d := range sysDirs {
		for _, n := range []string{"libonnxruntime.so", "libonnxruntime.so.1", "libonnxruntime.dylib"} {
			cand := filepath.Join(d, n)
			tried = append(tried, cand)
			if _, err := os.Stat(cand); err == nil {
				return cand, "system library", nil
			}
		}
	}
	// Versioned sonames, highest version wins.
	var versioned []string
	for _, d := range sysDirs {
		for _, pat := range []string{"libonnxruntime.so.*", "libonnxruntime.*.dylib"} {
			m, _ := filepath.Glob(filepath.Join(d, pat))
			versioned = append(versioned, m...)
		}
	}
	if len(versioned) > 0 {
		sort.Strings(versioned)
		return versioned[len(versioned)-1], "system library (versioned soname)", nil
	}

	return "", "", &ErrUnavailable{
		Reason: fmt.Sprintf("ONNX Runtime library not found (tried: %s)", strings.Join(tried, ", ")),
	}
}

func bundledORTNames(dir string) []string {
	return []string{
		filepath.Join(dir, "lib", "libonnxruntime.so"),
		filepath.Join(dir, "lib", "libonnxruntime.dylib"),
		filepath.Join(dir, "libonnxruntime.so"),
		filepath.Join(dir, "onnxruntime.dll"),
		// macOS app bundle: <app>/Contents/MacOS/<bin> -> Contents/Frameworks
		filepath.Join(dir, "..", "Frameworks", "libonnxruntime.dylib"),
	}
}

func systemLibDirs() []string {
	return []string{
		"/usr/lib/x86_64-linux-gnu", "/usr/lib/aarch64-linux-gnu",
		"/usr/lib", "/usr/local/lib", "/opt/homebrew/lib",
	}
}

// ---------- shared helpers ----------

func l2Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	n := math.Sqrt(sum)
	if n == 0 {
		return
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
}

// Cosine is the dot product of two unit vectors.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// ---------- deterministic fake ----------

// FakeEmbedder produces deterministic vectors with no model, no ONNX Runtime and
// no network, so every test that is not specifically about embedding quality can
// run anywhere.
//
// It is not random: features are hashed into the vector space (character
// trigrams plus whole words), which gives genuine lexical similarity structure.
// That matters because retrieval tests want "similar text scores higher" to hold
// without needing the real model — random vectors would make those tests
// meaningless.
type FakeEmbedder struct {
	dims int
}

// NewFakeEmbedder returns a deterministic embedder for tests.
func NewFakeEmbedder() *FakeEmbedder { return &FakeEmbedder{dims: EmbedDims} }

func (f *FakeEmbedder) Dims() int         { return f.dims }
func (f *FakeEmbedder) ModelName() string { return "fake-hashing-embedder" }
func (f *FakeEmbedder) Close() error      { return nil }

func (f *FakeEmbedder) EmbedPassages(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = f.vector(t)
	}
	return out, nil
}

func (f *FakeEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	// Deliberately does NOT include QueryPrefix in the hashed features: the
	// prefix is constant, so hashing it would add the same offset to every query
	// and dilute the signal. The real embedder does use it.
	return f.vector(text), nil
}

func (f *FakeEmbedder) vector(text string) []float32 {
	v := make([]float32, f.dims)
	norm := normalizeForNgrams(text)

	add := func(feature string, weight float32) {
		h := fnv.New64a()
		h.Write([]byte(feature))
		sum := h.Sum64()
		idx := int(sum % uint64(f.dims))
		// Sign from a different bit range so features do not all push one way.
		if sum&(1<<40) != 0 {
			v[idx] -= weight
		} else {
			v[idx] += weight
		}
	}

	for _, w := range strings.Fields(norm) {
		add("w:"+w, 1.0)
	}
	r := []rune(norm)
	for i := 0; i+3 <= len(r); i++ {
		add("t:"+string(r[i:i+3]), 0.5)
	}

	l2Normalize(v)
	// A zero vector would break cosine ranking; give empty input a fixed
	// direction instead.
	if allZero(v) {
		v[0] = 1
	}
	return v
}

func allZero(v []float32) bool {
	for _, x := range v {
		if x != 0 {
			return false
		}
	}
	return true
}
