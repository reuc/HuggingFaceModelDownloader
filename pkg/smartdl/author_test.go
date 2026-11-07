// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListAuthor(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models":
			if r.URL.Query().Get("author") != "acme" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			if r.URL.Query().Get("cursor") == "" {
				// Page 1 links page 2; a foreign-host link must not be followed.
				w.Header().Set("Link", fmt.Sprintf(`<%s/api/models?author=acme&cursor=p2>; rel="next"`, srvURL))
				_, _ = w.Write([]byte(`[{"id":"acme/llm","gated":"manual","safetensors":{"parameters":{"BF16":7000000000},"total":7000000000},
					"config":{"architectures":["LlamaForCausalLM"]}}]`))
				return
			}
			w.Header().Set("Link", `<https://evil.example/api/models?cursor=p3>; rel="next"`)
			_, _ = w.Write([]byte(`[{"id":"acme/llm-GGUF","gated":false,"gguf":{"total":7000000000,"architecture":"llama","context_length":8192}}]`))
		case "/api/datasets":
			if r.URL.Query().Get("author") != "acme" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":"acme/data","private":true,"downloads":5}]`))
		case "/api/users/empty/overview":
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	a := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()})
	ctx := context.Background()

	repos, err := a.ListAuthor(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 3 {
		t.Fatalf("got %d repos, want 3: %+v", len(repos), repos)
	}
	llm, gguf, data := repos[0], repos[1], repos[2]
	if llm.Repo != "acme/llm" || !llm.Gated || llm.Architecture != "LlamaForCausalLM" || llm.Parameters != "7.0B" || llm.Precision != "BF16" || !llm.Partial {
		t.Errorf("safetensors repo = %+v", llm)
	}
	if gguf.Architecture != "llama" || gguf.ContextLength != 8192 || gguf.Gated {
		t.Errorf("gguf repo = %+v", gguf)
	}
	if !data.IsDataset || !data.Private || data.Type != TypeDataset || data.Downloads != 5 {
		t.Errorf("dataset = %+v", data)
	}

	if repos, err := a.ListAuthor(ctx, "empty"); err != nil || len(repos) != 0 {
		t.Errorf("account without repos: %v, %v", repos, err)
	}
	if _, err := a.ListAuthor(ctx, "nobody"); !errors.Is(err, ErrAuthorNotFound) || !strings.Contains(err.Error(), "nobody") {
		t.Errorf("missing account: %v", err)
	}
}
