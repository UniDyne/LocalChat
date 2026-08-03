package main

import (
	"encoding/json"
	"testing"
)

func TestContextLengthFromModelInfo(t *testing.T) {
	cases := []struct {
		name string
		info map[string]any
		want int
	}{
		{"qwen float64", map[string]any{"general.architecture": "qwen3", "qwen3.context_length": float64(32768)}, 32768},
		{"llama float64", map[string]any{"llama.context_length": float64(8192)}, 8192},
		{"int value", map[string]any{"foo.context_length": 4096}, 4096},
		{"json.Number", map[string]any{"foo.context_length": json.Number("16384")}, 16384},
		{"absent", map[string]any{"general.architecture": "x"}, 0},
		{"nil map", nil, 0},
	}
	for _, c := range cases {
		if got := contextLengthFromModelInfo(c.info); got != c.want {
			t.Errorf("%s: contextLengthFromModelInfo = %d, want %d", c.name, got, c.want)
		}
	}
}
