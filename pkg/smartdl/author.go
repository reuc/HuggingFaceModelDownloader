// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/reuc/HuggingFaceModelDownloader/internal/hubtree"
)

// MaxAuthorRepos caps how many models and how many datasets ListAuthor
// returns (the largest accounts have a few thousand).
const MaxAuthorRepos = 10000

// authorPageSize is the listing page size; model entries carry config and
// GGUF metadata (chat templates), so pages are kept small.
const authorPageSize = 100

// ErrAuthorNotFound is returned when no user or organization has the name.
var ErrAuthorNotFound = fmt.Errorf("user or organization not found")

// AuthorRepo is one repo of a user or organization. Its RepoSummary holds
// what the Hub's listing reveals (no size, and context length only for
// GGUF); Partial is set until Summarize fills in the rest.
type AuthorRepo struct {
	RepoSummary
	Partial      bool   `json:"partial"`
	Private      bool   `json:"private,omitempty"`
	Gated        bool   `json:"gated,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Downloads    int64  `json:"downloads,omitempty"`
}

// hubListEntry is an element of /api/models or /api/datasets.
type hubListEntry struct {
	ID           string `json:"id"`
	Private      bool   `json:"private"`
	Gated        any    `json:"gated"` // false, "auto" or "manual"
	LastModified string `json:"lastModified"`
	Downloads    int64  `json:"downloads"`
	hubModelInfo
}

// ListAuthor lists every model and dataset of a user or organization,
// most recently modified first within each kind.
func (a *Analyzer) ListAuthor(ctx context.Context, author string) ([]AuthorRepo, error) {
	var out []AuthorRepo
	for _, kind := range []string{"models", "datasets"} {
		expand := []string{"lastModified", "private", "gated", "downloads"}
		if kind == "models" {
			expand = append(expand, "safetensors", "gguf", "config")
		}
		q := url.Values{"author": {author}, "limit": {fmt.Sprint(authorPageSize)}, "sort": {"lastModified"}, "direction": {"-1"}}
		q["expand[]"] = expand
		listURL := fmt.Sprintf("%s/api/%s?%s", a.endpoint, kind, q.Encode())

		err := hubtree.List(ctx, hubtree.Options{
			Client:    a.client,
			Authorize: a.addAuth,
			Status: func(resp *http.Response) error {
				return fmt.Errorf("list %s of %s: %s", kind, author, resp.Status)
			},
		}, listURL, MaxAuthorRepos, func(e hubListEntry) error {
			out = append(out, authorRepo(e, kind == "datasets"))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 && !a.authorExists(ctx, author) {
		return nil, fmt.Errorf("%w: %s", ErrAuthorNotFound, author)
	}
	return out, nil
}

// authorRepo converts a listing entry, summarizing what it reveals.
func authorRepo(e hubListEntry, isDataset bool) AuthorRepo {
	r := AuthorRepo{
		RepoSummary:  RepoSummary{Repo: e.ID, IsDataset: isDataset},
		Partial:      true,
		Private:      e.Private,
		LastModified: e.LastModified,
		Downloads:    e.Downloads,
	}
	if g, ok := e.Gated.(string); ok && g != "" {
		r.Gated = true
	} else if g, ok := e.Gated.(bool); ok {
		r.Gated = g
	}
	if isDataset {
		r.Type = TypeDataset
		return r
	}
	hub := &e.hubModelInfo
	switch {
	case hub.GGUF != nil && hub.GGUF.Architecture != "":
		r.Architecture = hub.GGUF.Architecture
	default:
		r.Architecture = configArchitecture(hub.Config)
	}
	switch {
	case hub.Safetensors != nil && hub.Safetensors.Total > 0:
		r.Parameters = formatParameterCount(hub.Safetensors.Total)
	case hub.GGUF != nil && hub.GGUF.Total > 0:
		r.Parameters = formatParameterCount(hub.GGUF.Total)
	}
	if hub.GGUF != nil && hub.GGUF.ContextLength > 0 {
		r.ContextLength = hub.GGUF.ContextLength
	} else {
		r.ContextLength = configContextLength(hub.Config)
	}
	r.Precision, r.PrecisionDetail = safetensorsPrecision(hub)
	return r
}

// authorExists reports whether a user or organization has the name; an
// account with no public repos is not an error.
func (a *Analyzer) authorExists(ctx context.Context, author string) bool {
	for _, kind := range []string{"users", "organizations"} {
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/%s/%s/overview", a.endpoint, kind, url.PathEscape(author)), nil)
		if err != nil {
			return false
		}
		a.addAuth(req)
		resp, err := a.client.Do(req)
		if err != nil {
			return true // can't tell: don't claim it is missing
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true
		}
		if resp.StatusCode != http.StatusNotFound {
			return true
		}
	}
	return false
}
