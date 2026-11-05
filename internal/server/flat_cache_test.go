// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A repo stored flat (serve --flatten) has no hub/ entry, only real files and
// a "storage: flat" manifest in models/<owner>/<name>. The Cache browser must
// still list it, show its files and be able to delete it.
func TestCacheRoutes_FlatRepo(t *testing.T) {
	cacheDir := t.TempDir()
	flat := filepath.Join(cacheDir, "models", "o", "r")
	if err := os.MkdirAll(filepath.Join(flat, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(flat, "config.json"), []byte(`{"a":1}`), 0o644)
	os.WriteFile(filepath.Join(flat, "sub", "model.bin"), make([]byte, 1000), 0o644)
	os.WriteFile(filepath.Join(flat, "hfd.yaml"), []byte("version: \"1.0\"\ntype: model\nrepo: o/r\nbranch: main\ncommit: 1111111111111111111111111111111111111111\nstorage: flat\ntotal_size: 1007\ntotal_files: 2\n"), 0o644)

	srv := New(Config{CacheDir: cacheDir})

	w := httptest.NewRecorder()
	srv.handleCacheList(w, httptest.NewRequest("GET", "/api/cache", nil))
	var list struct {
		Repos []CachedRepoInfo `json:"repos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("list: %v: %s", err, w.Body.String())
	}
	if len(list.Repos) != 1 || list.Repos[0].Repo != "o/r" || list.Repos[0].Size != 1007 || list.Repos[0].FileCount != 2 || list.Repos[0].Path != flat {
		t.Fatalf("list = %+v", list.Repos)
	}

	req := httptest.NewRequest("GET", "/api/cache/o/r", nil)
	req.SetPathValue("repo", "o/r")
	w = httptest.NewRecorder()
	srv.handleCacheInfo(w, req)
	var info CachedRepoInfo
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &info) != nil {
		t.Fatalf("info: %d %s", w.Code, w.Body.String())
	}
	if len(info.Files) != 2 || info.Size != 1007 || info.Commit != "1111111" {
		t.Fatalf("info = %+v", info)
	}

	req = httptest.NewRequest("DELETE", "/api/cache/o/r", nil)
	req.SetPathValue("repo", "o/r")
	w = httptest.NewRecorder()
	srv.handleCacheDelete(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(flat); !os.IsNotExist(err) {
		t.Fatalf("flat repo folder still exists (err=%v)", err)
	}
}

// makeHubRepo writes a minimal HF cache entry for o/r with one file per
// commit, refs/main pointing at the last one.
func makeHubRepo(t *testing.T, cacheDir string, commits ...string) string {
	t.Helper()
	repo := filepath.Join(cacheDir, "hub", "models--o--r")
	blobs := filepath.Join(repo, "blobs")
	os.MkdirAll(blobs, 0o755)
	for i, c := range commits {
		snap := filepath.Join(repo, "snapshots", c)
		os.MkdirAll(snap, 0o755)
		blob := fmt.Sprintf("%064d", i+1)
		os.WriteFile(filepath.Join(blobs, blob), []byte(c), 0o644)
		if err := os.Symlink(filepath.Join("..", "..", "blobs", blob), filepath.Join(snap, "config.json")); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(repo, "refs"), 0o755)
	os.WriteFile(filepath.Join(repo, "refs", "main"), []byte(commits[len(commits)-1]), 0o644)
	return repo
}

func postFlatten(srv *Server, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	srv.handleCacheFlatten(w, httptest.NewRequest("POST", "/api/cache/flatten", strings.NewReader(body)))
	return w
}

func TestCacheFlatten(t *testing.T) {
	c1 := strings.Repeat("1", 40)
	c2 := strings.Repeat("2", 40)
	cacheDir := t.TempDir()
	hubRepo := makeHubRepo(t, cacheDir, c1, c2)
	srv := New(Config{CacheDir: cacheDir})

	// Another revision cached: 409 listing it, nothing changed.
	w := postFlatten(srv, `{"repo":"o/r"}`)
	var conflict struct {
		Commit         string   `json:"commit"`
		OtherRevisions []string `json:"otherRevisions"`
	}
	json.Unmarshal(w.Body.Bytes(), &conflict)
	if w.Code != http.StatusConflict || conflict.Commit != c2 || len(conflict.OtherRevisions) != 1 || conflict.OtherRevisions[0] != c1 {
		t.Fatalf("without force: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(hubRepo); err != nil {
		t.Fatal("refused flatten changed the cache")
	}

	// A queued download of the repo blocks flattening.
	srv.jobs.mu.Lock()
	srv.jobs.jobs["x"] = &Job{ID: "x", Repo: "o/r", Status: JobStatusQueued}
	srv.jobs.mu.Unlock()
	if w := postFlatten(srv, `{"repo":"o/r","force":true}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "in progress") {
		t.Fatalf("with queued job: %d %s", w.Code, w.Body.String())
	}
	srv.jobs.mu.Lock()
	srv.jobs.jobs["x"].Status = JobStatusPaused
	srv.jobs.mu.Unlock()
	if w := postFlatten(srv, `{"repo":"o/r","force":true}`); w.Code != http.StatusConflict {
		t.Fatalf("with active job: %d %s", w.Code, w.Body.String())
	}
	srv.jobs.mu.Lock()
	delete(srv.jobs.jobs, "x")
	srv.jobs.mu.Unlock()

	w = postFlatten(srv, `{"repo":"o/r","force":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("force: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(hubRepo); !os.IsNotExist(err) {
		t.Fatal("hub/ entry still exists")
	}
	if got, _ := os.ReadFile(filepath.Join(cacheDir, "models", "o", "r", "config.json")); string(got) != c2 {
		t.Fatalf("config.json = %q, want the main revision's data", got)
	}

	if w := postFlatten(srv, `{"repo":"o/r"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("already flat: %d %s", w.Code, w.Body.String())
	}
	if w := postFlatten(srv, `{"repo":"o/missing"}`); w.Code != http.StatusNotFound {
		t.Fatalf("missing repo: %d %s", w.Code, w.Body.String())
	}
	if w := postFlatten(srv, `{"repo":"../x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad repo id: %d %s", w.Code, w.Body.String())
	}
}
