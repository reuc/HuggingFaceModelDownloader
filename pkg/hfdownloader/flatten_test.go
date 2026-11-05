// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// Flatten converts a repo downloaded the normal way, without downloading;
// a later --flatten download of it then skips every file.
func TestFlatten_ExistingRepo(t *testing.T) {
	for _, mode := range []string{"", "hardlink", "copy"} {
		t.Run("linkmode="+mode, func(t *testing.T) {
			weights := testPayload(300000)
			hub := &fakeHub{
				commit:        "1111111111111111111111111111111111111111",
				files:         map[string][]byte{"model.safetensors": weights},
				small:         map[string][]byte{"config.json": []byte(`{"v": 1}`)},
				treeAcceptSHA: true,
				fileAcceptSHA: true,
			}
			srv := httptest.NewServer(hub)
			defer srv.Close()
			root := t.TempDir()
			cfg := Settings{Endpoint: srv.URL, CacheDir: root, Retries: 1, LinkMode: mode}
			if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err != nil {
				t.Fatal(err)
			}
			rd, _ := NewHFCache(root, 0).Repo("o/r", RepoTypeModel)
			res, err := rd.Flatten(FlattenOptions{})
			if err != nil {
				t.Fatalf("Flatten: %v", err)
			}
			if res.Files != 2 || res.Commit != hub.commit || res.Bytes != int64(len(weights))+8 {
				t.Errorf("result = %+v", res)
			}
			if _, err := os.Lstat(rd.Path()); !os.IsNotExist(err) {
				t.Fatalf("hub/ entry still exists")
			}
			for name, want := range map[string][]byte{"model.safetensors": weights, "config.json": []byte(`{"v": 1}`)} {
				p := filepath.Join(rd.FriendlyPath(), name)
				fi, err := os.Lstat(p)
				if err != nil || !fi.Mode().IsRegular() {
					t.Fatalf("%s: not a regular file (%v)", name, err)
				}
				if got, _ := os.ReadFile(p); !bytes.Equal(got, want) {
					t.Fatalf("%s: wrong content", name)
				}
			}
			if !rd.IsFlat() {
				t.Fatal("IsFlat = false after Flatten")
			}
			if _, err := rd.Flatten(FlattenOptions{}); !errors.Is(err, ErrAlreadyFlat) {
				t.Errorf("second Flatten: %v, want ErrAlreadyFlat", err)
			}

			gets := hub.gets("/model.safetensors") + hub.gets("/raw/")
			cfg.Flatten = true
			if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err != nil {
				t.Fatal(err)
			}
			if got := hub.gets("/model.safetensors") + hub.gets("/raw/"); got != gets {
				t.Errorf("--flatten download after Flatten fetched %d file(s), want 0", got-gets)
			}
		})
	}
}

// With other revisions cached, Flatten refuses unless allowed to delete them,
// keeps every file of the chosen revision, and drops links to the others.
func TestFlatten_OtherRevisions(t *testing.T) {
	hub := &fakeHub{
		commit:        "1111111111111111111111111111111111111111",
		small:         map[string][]byte{"old.txt": []byte("old"), "config.json": []byte(`{"v": 1}`)},
		treeAcceptSHA: true,
		fileAcceptSHA: true,
	}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	root := t.TempDir()
	cfg := Settings{Endpoint: srv.URL, CacheDir: root, Retries: 1}
	Download(context.Background(), Job{Repo: "o/r"}, cfg, nil)
	hub.commit = "2222222222222222222222222222222222222222"
	hub.small = map[string][]byte{"config.json": []byte(`{"v": 2}`), "new.txt": []byte("new")}
	if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err != nil {
		t.Fatal(err)
	}

	rd, _ := NewHFCache(root, 0).Repo("o/r", RepoTypeModel)
	var other *OtherRevisionsError
	if _, err := rd.Flatten(FlattenOptions{}); !errors.As(err, &other) || len(other.Others) != 1 || other.Commit != hub.commit {
		t.Fatalf("Flatten: %v, want OtherRevisionsError for the old commit", err)
	}
	if _, err := os.Stat(rd.Path()); err != nil {
		t.Fatal("refused Flatten changed the cache")
	}
	res, err := rd.Flatten(FlattenOptions{DropOtherRevisions: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit != hub.commit || len(res.DroppedRevisions) != 1 {
		t.Errorf("result = %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(rd.FriendlyPath(), "config.json")); string(got) != `{"v": 2}` {
		t.Errorf("config.json = %q", got)
	}
	// old.txt was only in the old commit: its friendly link is gone.
	if _, err := os.Lstat(filepath.Join(rd.FriendlyPath(), "old.txt")); !os.IsNotExist(err) {
		t.Errorf("old.txt left in the friendly view (err=%v)", err)
	}
}

// A repo downloaded by Python's huggingface_hub (no hfd.yaml, small files'
// blobs named by git SHA-1) can be flattened too.
func TestFlatten_PythonCache(t *testing.T) {
	root := t.TempDir()
	cache := NewHFCache(root, 0)
	rd, _ := cache.Repo("o/r", RepoTypeModel)
	commit := "3333333333333333333333333333333333333333"
	small := []byte(`{"a": 1}`)
	smallBlob := gitBlobOID(small) // 40 hex: not a SHA-256
	os.MkdirAll(rd.BlobsDir(), 0o755)
	os.MkdirAll(filepath.Join(rd.SnapshotDir(commit), "sub"), 0o755)
	os.WriteFile(filepath.Join(rd.BlobsDir(), smallBlob), small, 0o644)
	os.Symlink(filepath.Join("..", "..", "blobs", smallBlob), filepath.Join(rd.SnapshotDir(commit), "config.json"))
	os.Symlink(filepath.Join("..", "..", "..", "blobs", smallBlob), filepath.Join(rd.SnapshotDir(commit), "sub", "same.json"))
	rd.WriteRef("main", commit)

	res, err := rd.Flatten(FlattenOptions{})
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}
	if res.Files != 2 {
		t.Errorf("files = %d, want 2", res.Files)
	}
	for _, name := range []string{"config.json", "sub/same.json"} {
		if got, err := os.ReadFile(filepath.Join(rd.FriendlyPath(), filepath.FromSlash(name))); err != nil || !bytes.Equal(got, small) {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	m, err := ReadManifest(filepath.Join(rd.FriendlyPath(), ManifestFilename))
	if err != nil || m.Storage != StorageFlat || m.Commit != commit || len(m.Files) != 2 {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	for _, f := range m.Files {
		if !validSHA256Hex(strings.TrimPrefix(f.Blob, "blobs/")) {
			t.Errorf("%s: blob %q is not a SHA-256", f.Name, f.Blob)
		}
	}
}

// Partial files of an unfinished download (e.g. a paused server job) are
// not deleted without DropOtherRevisions.
func TestFlatten_PartialDownload(t *testing.T) {
	root := t.TempDir()
	rd, _ := NewHFCache(root, 0).Repo("o/r", RepoTypeModel)
	commit := "4444444444444444444444444444444444444444"
	sha := strings.Repeat("a", 64)
	os.MkdirAll(rd.BlobsDir(), 0o755)
	os.MkdirAll(rd.SnapshotDir(commit), 0o755)
	os.WriteFile(rd.BlobPath(sha), []byte("x"), 0o644)
	os.Symlink(filepath.Join("..", "..", "blobs", sha), filepath.Join(rd.SnapshotDir(commit), "a.txt"))
	rd.WriteRef("main", commit)
	partial := filepath.Join(rd.BlobsDir(), "tmp-"+strings.Repeat("b", 64)+".part")
	os.WriteFile(partial, []byte("half"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(partial, old, old) // idle, like a paused job

	var other *OtherRevisionsError
	if _, err := rd.Flatten(FlattenOptions{}); !errors.As(err, &other) || other.Partial != 1 {
		t.Fatalf("Flatten: %v, want OtherRevisionsError with 1 partial file", err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatal("partial file deleted")
	}
	if _, err := rd.Flatten(FlattenOptions{DropOtherRevisions: true}); err != nil {
		t.Fatal(err)
	}
}
