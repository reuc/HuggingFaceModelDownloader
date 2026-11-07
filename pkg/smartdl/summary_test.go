// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSummaryHub serves model trees, raw files and model info; datasets is
// the set of repos that also exist as datasets.
func fakeSummaryHub(t *testing.T, trees map[string][]map[string]any, raw map[string]string, modelInfo map[string]string, datasets map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/api/models/") && strings.HasSuffix(p, "/tree/main"):
			repo := strings.TrimSuffix(strings.TrimPrefix(p, "/api/models/"), "/tree/main")
			tree, ok := trees[repo]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(tree)
		case strings.HasPrefix(p, "/api/datasets/") && strings.HasSuffix(p, "/tree/main"):
			repo := strings.TrimSuffix(strings.TrimPrefix(p, "/api/datasets/"), "/tree/main")
			if !datasets[repo] {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": "data/train.parquet", "size": 100}})
		case strings.HasSuffix(p, "/refs"):
			_, _ = w.Write([]byte(`{"branches":[{"name":"main","targetCommit":"abc"}],"tags":[]}`))
		case strings.HasPrefix(p, "/api/models/"):
			body, ok := modelInfo[strings.TrimPrefix(p, "/api/models/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			body, ok := raw[p]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
		}
	}))
}

func TestSummarize(t *testing.T) {
	trees := map[string][]map[string]any{
		"o/llm": {
			{"type": "file", "path": "config.json", "size": 300},
			{"type": "file", "path": "model.safetensors", "size": 16_000_000_000},
		},
		"o/llm-gguf": {
			{"type": "file", "path": "llm-8B-Q4_K_M.gguf", "size": 5_000_000_000},
			{"type": "file", "path": "llm-8B-Q5_K_M.gguf", "size": 6_000_000_000},
			{"type": "file", "path": "llm-8B-Q6_K.gguf", "size": 7_000_000_000},
			{"type": "file", "path": "llm-8B-Q8_0.gguf", "size": 8_000_000_000},
		},
	}
	raw := map[string]string{
		// sliding_window must not be reported as the context length.
		"/o/llm/raw/main/config.json": `{"architectures":["LlamaForCausalLM"],"model_type":"llama","hidden_size":4096,"num_hidden_layers":32,
			"max_position_embeddings":131072,"sliding_window":4096,"torch_dtype":"bfloat16"}`,
	}
	modelInfo := map[string]string{
		"o/llm":      `{"safetensors":{"parameters":{"BF16":8030261248,"F32":1024},"total":8030262272}}`,
		"o/llm-gguf": `{"gguf":{"total":8030261248,"architecture":"llama","context_length":131072}}`,
	}
	srv := fakeSummaryHub(t, trees, raw, modelInfo, map[string]bool{"o/llm": true, "o/data": true})
	defer srv.Close()
	a := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()})
	ctx := context.Background()

	s, err := a.Summarize(ctx, "o/llm", "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Ambiguous || s.IsDataset {
		t.Errorf("model+dataset repo: ambiguous=%v dataset=%v, want model summarized and flagged", s.Ambiguous, s.IsDataset)
	}
	if s.Architecture != "LlamaForCausalLM" || s.Parameters != "8.0B" || s.ContextLength != 131072 ||
		s.Precision != "BF16" || s.PrecisionDetail != "BF16, F32" || s.Size != 16_000_000_300 {
		t.Errorf("transformers summary = %+v", s)
	}

	s, err = a.Summarize(ctx, "o/llm-gguf", "model")
	if err != nil {
		t.Fatal(err)
	}
	if s.Architecture != "llama" || s.Parameters != "8.0B" || s.ContextLength != 131072 ||
		s.Precision != "GGUF (4 quants)" || !strings.Contains(s.PrecisionDetail, "Q4_K_M") {
		t.Errorf("gguf summary = %+v", s)
	}

	s, err = a.Summarize(ctx, "o/data", "dataset")
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsDataset || s.Architecture != "" || s.Size != 100 {
		t.Errorf("dataset summary = %+v", s)
	}

	if _, err := a.Summarize(ctx, "o/missing", ""); err == nil {
		t.Error("missing repo: want error")
	}
}

func TestConfigContextLength_NestedTextConfig(t *testing.T) {
	cfg := map[string]interface{}{"text_config": map[string]interface{}{"max_position_embeddings": 32768.0}}
	if got := configContextLength(cfg); got != 32768 {
		t.Errorf("got %d, want 32768", got)
	}
}
