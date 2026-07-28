package memory

import (
	"context"
	"fmt"

	"github.com/ollama/ollama/api"
)

// OllamaEmbedder is the fallback embedder, for the case where a platform's ONNX
// Runtime build misbehaves or a user cannot provision the model file.
//
// Not the primary path: it reintroduces a network dependency (the configured
// endpoint may be on the LAN rather than localhost) and gives up the throughput
// advantage of in-process inference, which matters most for the per-sentence
// embedding the Leiden chunker needs. It exists so a working memory system is
// still reachable when the local path is not.
type OllamaEmbedder struct {
	client    *api.Client
	model     string
	dims      int
	batchSize int
}

// NewOllamaEmbedder builds the fallback. dims must match the model's actual width;
// a mismatch is caught by the store's dimension check rather than silently
// producing unusable vectors.
func NewOllamaEmbedder(client *api.Client, model string, dims int) (*OllamaEmbedder, error) {
	if client == nil {
		return nil, &ErrUnavailable{Reason: "no Ollama client configured"}
	}
	if model == "" {
		return nil, &ErrUnavailable{Reason: "no Ollama embedding model configured"}
	}
	if dims <= 0 {
		dims = EmbedDims
	}
	return &OllamaEmbedder{client: client, model: model, dims: dims, batchSize: DefaultBatchSize}, nil
}

func (e *OllamaEmbedder) Dims() int         { return e.dims }
func (e *OllamaEmbedder) ModelName() string { return "ollama:" + e.model }
func (e *OllamaEmbedder) Close() error      { return nil }

func (e *OllamaEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vs, err := e.EmbedPassages(ctx, []string{QueryPrefix + text})
	if err != nil {
		return nil, err
	}
	return vs[0], nil
}

// EmbedPassages batches through Ollama's /api/embed, which accepts a []string
// input and returns [][]float32.
func (e *OllamaEmbedder) EmbedPassages(ctx context.Context, texts []string) ([][]float32, error) {
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
		batch := texts[start:end]

		resp, err := e.client.Embed(ctx, &api.EmbedRequest{
			Model: e.model,
			Input: batch,
		})
		if err != nil {
			return nil, fmt.Errorf("ollama embed: %w", err)
		}
		if len(resp.Embeddings) != len(batch) {
			return nil, fmt.Errorf("ollama returned %d embeddings for %d inputs",
				len(resp.Embeddings), len(batch))
		}
		for i, v := range resp.Embeddings {
			if len(v) != e.dims {
				return nil, fmt.Errorf("ollama embedding %d has %d dims, want %d "+
					"(model %q does not match the configured dimension)",
					i, len(v), e.dims, e.model)
			}
			cp := make([]float32, len(v))
			copy(cp, v)
			// Ollama does not guarantee normalization; the store and all cosine
			// maths assume unit vectors.
			l2Normalize(cp)
			out = append(out, cp)
		}
	}
	return out, nil
}
