// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// With Flatten, a finished download is only real files in models/<owner>/<name>
// (no links, no hub/ entry), and later downloads of the same repo skip files
// that are already there unchanged.
func TestDownload_FlattenMovesFilesOutOfHub(t *testing.T) {
	weights := testPayload(300000)
	hub := &fakeHub{
		commit: "1111111111111111111111111111111111111111",
		// Two names with identical content share one blob: one is moved,
		// the other must be copied.
		files:         map[string][]byte{"model.safetensors": weights, "copy/model.safetensors": weights},
		small:         map[string][]byte{"config.json": []byte(`{"v": 1}`), "README.md": []byte("readme")},
		treeAcceptSHA: true,
		fileAcceptSHA: true,
	}
	srv := httptest.NewServer(hub)
	defer srv.Close()

	root := t.TempDir()
	cfg := Settings{Endpoint: srv.URL, CacheDir: root, Concurrency: 4, MaxActiveDownloads: 4, Retries: 1, Flatten: true}
	download := func() {
		t.Helper()
		if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err != nil {
			t.Fatalf("Download: %v", err)
		}
	}
	flat := filepath.Join(root, "models", "o", "r")
	check := func(want map[string][]byte) {
		t.Helper()
		if _, err := os.Lstat(filepath.Join(root, "hub", "models--o--r")); !os.IsNotExist(err) {
			t.Fatalf("hub/ entry still exists (err=%v)", err)
		}
		for name, data := range want {
			p := filepath.Join(flat, filepath.FromSlash(name))
			fi, err := os.Lstat(p)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !fi.Mode().IsRegular() {
				t.Fatalf("%s is not a regular file (mode %v)", name, fi.Mode())
			}
			got, _ := os.ReadFile(p)
			if !bytes.Equal(got, data) {
				t.Fatalf("%s: wrong content", name)
			}
		}
		m, err := ReadManifest(filepath.Join(flat, ManifestFilename))
		if err != nil {
			t.Fatalf("manifest: %v", err)
		}
		if m.Storage != StorageFlat || m.RepoPath != "" {
			t.Fatalf("manifest storage=%q repo_path=%q, want flat and empty", m.Storage, m.RepoPath)
		}
	}

	download()
	check(map[string][]byte{"model.safetensors": weights, "copy/model.safetensors": weights, "config.json": []byte(`{"v": 1}`), "README.md": []byte("readme")})
	weightGets, rawGets := hub.gets("/model.safetensors"), hub.gets("/raw/")

	// Same commit again: everything is already in place, nothing downloads.
	download()
	if hub.gets("/model.safetensors") != weightGets || hub.gets("/raw/") != rawGets {
		t.Errorf("re-run downloaded files: weights GETs %d->%d, raw GETs %d->%d",
			weightGets, hub.gets("/model.safetensors"), rawGets, hub.gets("/raw/"))
	}
	check(map[string][]byte{"model.safetensors": weights, "config.json": []byte(`{"v": 1}`)})

	// The branch moves: only the changed file downloads.
	hub.commit = "2222222222222222222222222222222222222222"
	hub.small = map[string][]byte{"config.json": []byte(`{"v": 2}`), "README.md": []byte("readme")}
	download()
	if hub.gets("/model.safetensors") != weightGets {
		t.Errorf("unchanged weights downloaded again")
	}
	if got := hub.gets("/raw/") - rawGets; got != 1 {
		t.Errorf("branch update: %d raw GETs, want 1 (config.json)", got)
	}
	check(map[string][]byte{"model.safetensors": weights, "config.json": []byte(`{"v": 2}`), "README.md": []byte("readme")})

	// The rest of the tool sees the flat repo.
	cache := NewHFCache(root, 0)
	repos, err := cache.ListFlatRepos()
	if err != nil || len(repos) != 1 || repos[0].RepoID() != "o/r" {
		t.Fatalf("ListFlatRepos = %v, %v", repos, err)
	}
	rd, _ := cache.Repo("o/r", RepoTypeModel)
	dest := filepath.Join(t.TempDir(), "out")
	res, err := rd.Export(dest, ExportOptions{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Files != 4 || res.Commit != hub.commit {
		t.Errorf("Export: %d files at %s, want 4 at %s", res.Files, res.Commit, hub.commit)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "config.json")); string(got) != `{"v": 2}` {
		t.Errorf("exported config.json = %q", got)
	}
}

// Without Flatten nothing changes: the friendly view holds links and hub/ is
// kept.
func TestDownload_NoFlattenKeepsHub(t *testing.T) {
	hub := &fakeHub{
		commit:        "1111111111111111111111111111111111111111",
		small:         map[string][]byte{"config.json": []byte(`{}`)},
		treeAcceptSHA: true,
		fileAcceptSHA: true,
	}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	root := t.TempDir()
	cfg := Settings{Endpoint: srv.URL, CacheDir: root, Retries: 1}
	if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "hub", "models--o--r", "blobs")); err != nil {
		t.Fatalf("hub/ entry missing: %v", err)
	}
	if rd, _ := NewHFCache(root, 0).Repo("o/r", RepoTypeModel); rd.IsFlat() {
		t.Fatal("repo reported as flat")
	}
}

func TestValidate_FlattenNeedsFriendlyView(t *testing.T) {
	err := Download(context.Background(), Job{Repo: "o/r"}, Settings{Flatten: true, NoFriendlyView: true}, nil)
	if err == nil {
		t.Fatal("Flatten with NoFriendlyView: want an error")
	}
}
