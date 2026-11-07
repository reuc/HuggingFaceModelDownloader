// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gitBlobOID is the git blob id the Hub reports as a file's "oid".
func gitBlobOID(data []byte) string {
	// deepcode ignore InsecureHash/test: computes a git blob id (SHA-1 by definition) for a test fixture
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// gets counts GET requests whose path contains substr.
func (h *fakeHub) gets(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, p := range h.paths {
		if strings.HasPrefix(p, "GET ") && strings.Contains(p, substr) {
			n++
		}
	}
	return n
}

// When a branch moves to a new commit, only new or changed files are
// downloaded; unchanged ones (LFS and non-LFS) are linked into the new
// snapshot. Non-LFS files used to be downloaded again on every run.
func TestDownload_BranchUpdateDownloadsOnlyChanges(t *testing.T) {
	weights := testPayload(300000)
	hub := &fakeHub{
		commit:        "1111111111111111111111111111111111111111",
		files:         map[string][]byte{"model.safetensors": weights},
		small:         map[string][]byte{"config.json": []byte(`{"v": 1}`), "README.md": []byte("readme")},
		treeAcceptSHA: true,
		fileAcceptSHA: true,
	}
	srv := httptest.NewServer(hub)
	defer srv.Close()

	cache := t.TempDir()
	cfg := Settings{Endpoint: srv.URL, CacheDir: cache, Concurrency: 4, MaxActiveDownloads: 4, Retries: 1}
	download := func() {
		t.Helper()
		if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err != nil {
			t.Fatalf("Download: %v", err)
		}
	}
	gets := hub.gets

	download()
	if gets("/raw/") != 2 || gets("/model.safetensors") == 0 {
		t.Fatalf("first run: raw GETs %d, weights GETs %d", gets("/raw/"), gets("/model.safetensors"))
	}
	weightGets := gets("/model.safetensors")

	// Same commit again: nothing to download.
	download()
	if gets("/raw/") != 2 || gets("/model.safetensors") != weightGets {
		t.Errorf("re-run of the same commit downloaded files: raw GETs %d, weights GETs %d", gets("/raw/"), gets("/model.safetensors"))
	}

	// The branch moves: config.json changes (same size), a file is added.
	hub.commit = "2222222222222222222222222222222222222222"
	hub.small = map[string][]byte{"config.json": []byte(`{"v": 2}`), "README.md": []byte("readme"), "new.txt": []byte("new")}
	download()
	if n := gets("/config.json"); n != 2 {
		t.Errorf("changed config.json fetched %d times in all, want 2", n)
	}
	if n := gets("/new.txt"); n != 1 {
		t.Errorf("new file fetched %d times, want 1", n)
	}
	if n := gets("/README.md"); n != 1 {
		t.Errorf("unchanged README.md fetched %d times, want 1", n)
	}
	if gets("/model.safetensors") != weightGets {
		t.Error("unchanged weights downloaded again")
	}

	repo := filepath.Join(cache, "hub", "models--o--r")
	snap := filepath.Join(repo, "snapshots", hub.commit)
	want := map[string][]byte{"model.safetensors": weights, "config.json": []byte(`{"v": 2}`), "README.md": []byte("readme"), "new.txt": []byte("new")}
	for path, data := range want {
		if got, err := os.ReadFile(filepath.Join(snap, path)); err != nil || !bytes.Equal(got, data) {
			t.Errorf("new snapshot %s: err=%v content=%q", path, err, got)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(snap, "..", "1111111111111111111111111111111111111111", "config.json")); string(got) != `{"v": 1}` {
		t.Errorf("old snapshot config.json = %q, want it kept", got)
	}
	if ref, _ := os.ReadFile(filepath.Join(repo, "refs", "main")); string(bytes.TrimSpace(ref)) != hub.commit {
		t.Errorf("refs/main = %q, want %s", ref, hub.commit)
	}
}

// In --output (flat) mode a changed non-LFS file of the same size used to be
// skipped on a size match; the git oid now decides.
func TestDownload_LegacyModeRefetchesChangedSameSizeFile(t *testing.T) {
	hub := &fakeHub{
		commit:        "1111111111111111111111111111111111111111",
		small:         map[string][]byte{"config.json": []byte(`{"v": 1}`), "README.md": []byte("readme")},
		treeAcceptSHA: true,
		fileAcceptSHA: true,
	}
	srv := httptest.NewServer(hub)
	defer srv.Close()

	out := t.TempDir()
	cfg := Settings{Endpoint: srv.URL, OutputDir: out, Concurrency: 2, MaxActiveDownloads: 2, Retries: 1}
	for i, cfgJSON := range []string{`{"v": 1}`, `{"v": 2}`} {
		hub.small["config.json"] = []byte(cfgJSON)
		if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		got, err := os.ReadFile(filepath.Join(destinationBase(Job{Repo: "o/r"}, cfg), "config.json"))
		if err != nil || string(got) != cfgJSON {
			t.Errorf("run %d: config.json = %q (err %v), want %q", i, got, err, cfgJSON)
		}
	}
	if n := hub.gets("/README.md"); n != 1 {
		t.Errorf("unchanged README.md fetched %d times, want 1", n)
	}
}
