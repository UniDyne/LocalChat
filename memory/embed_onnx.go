package memory

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/sugarme/tokenizer"
	ort "github.com/yalue/onnxruntime_go"
)

// MaxSeqLen is the model's hard window. ONNX Runtime truncates silently past it,
// which is why the chunker splits the budget rather than assuming it has all 512
// (see chunker.go).
const MaxSeqLen = 512

// DefaultBatchSize is the batch that measured fastest in Phase 0: 46 texts/sec at
// 32, versus 24 at batch 1 and no further gain at 64.
const DefaultBatchSize = 32

// ortEnv guards the process-global ONNX Runtime environment. Initializing it twice
// is an error, so every embedder shares one initialization.
var (
	ortOnce sync.Once
	ortPath string
	ortHow  string
	ortErr  error
)

// initORT initializes the ONNX Runtime environment exactly once per process.
func initORT() (string, string, error) {
	ortOnce.Do(func() {
		ortPath, ortHow, ortErr = FindORT()
		if ortErr != nil {
			return
		}
		ort.SetSharedLibraryPath(ortPath)
		if err := ort.InitializeEnvironment(); err != nil {
			// The most likely cause is a version floor: the binding compiles in a
			// required API version and an older library rejects it. Say so,
			// because "initialization failed" alone is very hard to act on.
			ortErr = &ErrUnavailable{
				Reason: fmt.Sprintf("ONNX Runtime at %s could not be initialized "+
					"(often means the library is older than this build requires; "+
					"bundle a newer libonnxruntime or set ONNXRUNTIME_LIB)", ortPath),
				Err: err,
			}
		}
	})
	return ortPath, ortHow, ortErr
}

// ONNXEmbedder runs bge-small-en-v1.5 in-process.
type ONNXEmbedder struct {
	// mu serializes tokenization and inference.
	//
	// sugarme/tokenizer is not documented as goroutine-safe, and serializing the
	// whole call also keeps CPU use predictable: intra-op threads are already
	// capped so embedding does not fight the local LLM for cores, and letting
	// several batches run concurrently would defeat that cap.
	mu      sync.Mutex
	tk      *tokenizer.Tokenizer
	session *ort.DynamicAdvancedSession
	inputs  []string
	output  string

	modelName string
	batchSize int
}

// ONNXConfig configures the in-process embedder. Zero values are filled with the
// measured defaults.
type ONNXConfig struct {
	// ModelDir overrides model resolution.
	ModelDir string
	// IntraOpThreads caps ONNX Runtime's internal parallelism. 0 means
	// min(4, NumCPU) — deliberately not "all cores", which would contend with the
	// UI and the local LLM.
	IntraOpThreads int
	// BatchSize for EmbedPassages. 0 means DefaultBatchSize.
	BatchSize int
}

// NewONNXEmbedder loads the model and prepares a session. Returns an
// *ErrUnavailable when the runtime or the model cannot be found, which callers
// should treat as "memory disabled with a reason" rather than a fatal error.
func NewONNXEmbedder(cfg ONNXConfig) (*ONNXEmbedder, error) {
	// Resolve the model first: it is the cheaper check, the likelier failure, and
	// "model not provisioned" is far more actionable than a runtime error. It also
	// keeps this deterministic — ORT initialization is a process-global
	// sync.Once, so its outcome can depend on what ran before.
	var paths ModelPaths
	if cfg.ModelDir != "" {
		paths = ModelPaths{
			Dir:           cfg.ModelDir,
			ModelONNX:     filepath.Join(cfg.ModelDir, "model.onnx"),
			TokenizerJSON: filepath.Join(cfg.ModelDir, "tokenizer.json"),
		}
		if _, err := os.Stat(paths.ModelONNX); err != nil {
			return nil, &ErrUnavailable{
				Reason: fmt.Sprintf("embedding model not found at %s", paths.ModelONNX),
			}
		}
	} else {
		p, err := FindModel()
		if err != nil {
			return nil, err
		}
		paths = p
	}

	if _, how, err := initORT(); err != nil {
		return nil, err
	} else {
		slog.Info("onnx runtime loaded", "path", ortPath, "via", how)
	}

	tk, err := LoadTokenizer(paths.TokenizerJSON)
	if err != nil {
		return nil, &ErrUnavailable{Reason: "tokenizer could not be loaded", Err: err}
	}

	ins, outs, err := ort.GetInputOutputInfo(paths.ModelONNX)
	if err != nil {
		return nil, &ErrUnavailable{Reason: "model graph could not be read", Err: err}
	}
	inputNames := make([]string, 0, len(ins))
	for _, i := range ins {
		inputNames = append(inputNames, i.Name)
	}
	// Verified in Phase 0: inputs are input_ids, attention_mask, token_type_ids;
	// output is last_hidden_state [-1,-1,384]. Read rather than assumed, since a
	// re-exported model could differ.
	output := ""
	for _, o := range outs {
		if o.Name == "last_hidden_state" {
			output = o.Name
		}
	}
	if output == "" && len(outs) > 0 {
		output = outs[0].Name
	}
	if output == "" {
		return nil, &ErrUnavailable{Reason: "model declares no outputs"}
	}

	threads := cfg.IntraOpThreads
	if threads <= 0 {
		threads = runtime.NumCPU()
		if threads > 4 {
			threads = 4
		}
	}
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, &ErrUnavailable{Reason: "session options", Err: err}
	}
	defer opts.Destroy()
	if err := opts.SetIntraOpNumThreads(threads); err != nil {
		return nil, &ErrUnavailable{Reason: "set intra-op threads", Err: err}
	}
	if err := opts.SetInterOpNumThreads(1); err != nil {
		return nil, &ErrUnavailable{Reason: "set inter-op threads", Err: err}
	}

	sess, err := ort.NewDynamicAdvancedSession(paths.ModelONNX, inputNames, []string{output}, opts)
	if err != nil {
		return nil, &ErrUnavailable{Reason: "ONNX session could not be created", Err: err}
	}

	batch := cfg.BatchSize
	if batch <= 0 {
		batch = DefaultBatchSize
	}
	return &ONNXEmbedder{
		tk:        tk,
		session:   sess,
		inputs:    inputNames,
		output:    output,
		modelName: ModelDirName,
		batchSize: batch,
	}, nil
}

func (e *ONNXEmbedder) Dims() int         { return EmbedDims }
func (e *ONNXEmbedder) ModelName() string { return e.modelName }

func (e *ONNXEmbedder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session != nil {
		e.session.Destroy()
		e.session = nil
	}
	return nil
}

func (e *ONNXEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vs, err := e.EmbedPassages(ctx, []string{QueryPrefix + text})
	if err != nil {
		return nil, err
	}
	return vs[0], nil
}

// EmbedPassages embeds texts in batches, CLS-pooling and L2-normalizing each.
func (e *ONNXEmbedder) EmbedPassages(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += e.batchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + e.batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vs, err := e.embedOneBatch(texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	return out, nil
}

func (e *ONNXEmbedder) embedOneBatch(texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session == nil {
		return nil, fmt.Errorf("embedder is closed")
	}

	rows := make([][3][]int64, len(texts))
	maxLen := 0
	for i, t := range texts {
		ids, mask, tids, err := e.tokenizeLocked(t)
		if err != nil {
			return nil, err
		}
		rows[i] = [3][]int64{ids, mask, tids}
		if len(ids) > maxLen {
			maxLen = len(ids)
		}
	}
	if maxLen == 0 {
		// Every input was empty; return unit vectors rather than failing.
		out := make([][]float32, len(texts))
		for i := range out {
			v := make([]float32, EmbedDims)
			v[0] = 1
			out[i] = v
		}
		return out, nil
	}

	b := len(texts)
	flatIDs := make([]int64, b*maxLen)
	flatMask := make([]int64, b*maxLen)
	flatType := make([]int64, b*maxLen)
	for i, r := range rows {
		copy(flatIDs[i*maxLen:], r[0])
		copy(flatMask[i*maxLen:], r[1])
		copy(flatType[i*maxLen:], r[2])
	}

	shape := ort.NewShape(int64(b), int64(maxLen))
	byName := map[string][]int64{
		"input_ids":      flatIDs,
		"attention_mask": flatMask,
		"token_type_ids": flatType,
	}
	inputVals := make([]ort.Value, 0, len(e.inputs))
	for _, name := range e.inputs {
		data, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("model requires unknown input %q", name)
		}
		t, err := ort.NewTensor(shape, data)
		if err != nil {
			return nil, err
		}
		defer t.Destroy()
		inputVals = append(inputVals, t)
	}

	outTensor, err := ort.NewEmptyTensor[float32](ort.NewShape(int64(b), int64(maxLen), EmbedDims))
	if err != nil {
		return nil, err
	}
	defer outTensor.Destroy()

	if err := e.session.Run(inputVals, []ort.Value{outTensor}); err != nil {
		return nil, fmt.Errorf("onnx run: %w", err)
	}

	raw := outTensor.GetData()
	out := make([][]float32, b)
	for i := 0; i < b; i++ {
		// CLS pooling — token 0. Confirmed from the model's own
		// 1_Pooling/config.json: pooling_mode_cls_token=true, mean/max false.
		// Padding-invariance is asserted in the tests, which is what proves the
		// pooling and mask handling are right.
		start := i * maxLen * EmbedDims
		v := make([]float32, EmbedDims)
		copy(v, raw[start:start+EmbedDims])
		l2Normalize(v)
		out[i] = v
	}
	return out, nil
}

// tokenizeLocked encodes one text, truncating to MaxSeqLen while preserving the
// trailing [SEP]. A naive slice of the id array would drop it, which the parity
// fixture's truncation case guards against.
//
// Caller must hold e.mu.
func (e *ONNXEmbedder) tokenizeLocked(text string) (ids, mask, typeIDs []int64, err error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil, nil, nil
	}
	enc, err := e.tk.EncodeSingle(text, true)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tokenize: %w", err)
	}
	n := len(enc.Ids)
	if n > MaxSeqLen {
		sep := enc.Ids[n-1]
		ids = make([]int64, MaxSeqLen)
		mask = make([]int64, MaxSeqLen)
		typeIDs = make([]int64, MaxSeqLen)
		for i := 0; i < MaxSeqLen-1; i++ {
			ids[i] = int64(enc.Ids[i])
			mask[i] = 1
		}
		ids[MaxSeqLen-1] = int64(sep)
		mask[MaxSeqLen-1] = 1
		return ids, mask, typeIDs, nil
	}
	ids = make([]int64, n)
	mask = make([]int64, n)
	typeIDs = make([]int64, n)
	for i := range enc.Ids {
		ids[i] = int64(enc.Ids[i])
		mask[i] = int64(enc.AttentionMask[i])
		typeIDs[i] = int64(enc.TypeIds[i])
	}
	return ids, mask, typeIDs, nil
}
