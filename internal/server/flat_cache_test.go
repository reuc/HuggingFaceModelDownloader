// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
