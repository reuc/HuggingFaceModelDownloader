// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reuc/HuggingFaceModelDownloader/internal/testutil"
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
	if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err != nil {
		t.Fatal(err)
	}
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
	testutil.MkdirAll(t, rd.BlobsDir(), 0o755)
	testutil.MkdirAll(t, filepath.Join(rd.SnapshotDir(commit), "sub"), 0o755)
	testutil.WriteFile(t, filepath.Join(rd.BlobsDir(), smallBlob), small, 0o644)
	testutil.Symlink(t, filepath.Join("..", "..", "blobs", smallBlob), filepath.Join(rd.SnapshotDir(commit), "config.json"))
	testutil.Symlink(t, filepath.Join("..", "..", "..", "blobs", smallBlob), filepath.Join(rd.SnapshotDir(commit), "sub", "same.json"))
	if err := rd.WriteRef("main", commit); err != nil {
		t.Fatal(err)
	}

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
	testutil.MkdirAll(t, rd.BlobsDir(), 0o755)
	testutil.MkdirAll(t, rd.SnapshotDir(commit), 0o755)
	testutil.WriteFile(t, rd.BlobPath(sha), []byte("x"), 0o644)
	testutil.Symlink(t, filepath.Join("..", "..", "blobs", sha), filepath.Join(rd.SnapshotDir(commit), "a.txt"))
	if err := rd.WriteRef("main", commit); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(rd.BlobsDir(), "tmp-"+strings.Repeat("b", 64)+".part")
	testutil.WriteFile(t, partial, []byte("half"), 0o644)
	old := time.Now().Add(-time.Hour)
	testutil.Chtimes(t, partial, old, old) // idle, like a paused job

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

// makeSnapshot writes an HF cache entry for o/r at commit with the given
// files (each its own blob, linked from the snapshot) and refs/main.
func makeSnapshot(t *testing.T, rd *RepoDir, commit string, files map[string]string) {
	t.Helper()
	i := 0
	for name, data := range files {
		i++
		sha := fmt.Sprintf("%064x", i)
		testutil.MkdirAll(t, rd.BlobsDir(), 0o755)
		testutil.WriteFile(t, rd.BlobPath(sha), []byte(data), 0o644)
		link := filepath.Join(rd.SnapshotDir(commit), filepath.FromSlash(name))
		testutil.MkdirAll(t, filepath.Dir(link), 0o755)
		target, _ := filepath.Rel(filepath.Dir(link), rd.BlobPath(sha))
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := rd.WriteRef("main", commit); err != nil {
		t.Fatal(err)
	}
}

// Copilot review: a blob another process is still downloading has no data
// to move yet, and that download still needs hub/. A --flatten download must
// fail instead of treating it as done.
func TestDownload_FlattenRefusesBlobDownloadingElsewhere(t *testing.T) {
	weights := testPayload(300000)
	hub := &fakeHub{
		commit:        "1111111111111111111111111111111111111111",
		files:         map[string][]byte{"model.safetensors": weights},
		treeAcceptSHA: true,
		fileAcceptSHA: true,
	}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	root := t.TempDir()
	rd, _ := NewHFCache(root, 0).Repo("o/r", RepoTypeModel)
	sum := sha256.Sum256(weights)
	sha := hex.EncodeToString(sum[:])
	testutil.MkdirAll(t, rd.BlobsDir(), 0o755)
	testutil.WriteFile(t, rd.IncompletePath(sha), []byte("partial"), 0o644)
	meta, _ := json.Marshal(IncompleteMeta{PID: os.Getpid(), StartedAt: time.Now(), SHA256: sha})
	testutil.WriteFile(t, rd.IncompleteMetaPath(sha), meta, 0o644)

	cfg := Settings{Endpoint: srv.URL, CacheDir: root, Retries: 1, Flatten: true}
	err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "another process") {
		t.Fatalf("Download: %v, want an error about the other download", err)
	}
	if _, err := os.Stat(rd.IncompletePath(sha)); err != nil {
		t.Fatal("the other download's data was removed")
	}
}

// Copilot review: a failure while placing files must leave the cache as it
// was, so a retry can still find every file.
func TestFlatten_FailureLeavesCacheIntact(t *testing.T) {
	root := t.TempDir()
	rd, _ := NewHFCache(root, 0).Repo("o/r", RepoTypeModel)
	commit := strings.Repeat("5", 40)
	makeSnapshot(t, rd, commit, map[string]string{"a.txt": "A", "sub/b.txt": "B"})
	// A file where the folder sub/ must go: a.txt stages fine, sub/b.txt fails.
	testutil.MkdirAll(t, rd.FriendlyPath(), 0o755)
	obstacle := filepath.Join(rd.FriendlyPath(), "sub")
	testutil.WriteFile(t, obstacle, []byte("x"), 0o644)

	if _, err := rd.Flatten(FlattenOptions{}); err == nil {
		t.Fatal("Flatten: want an error")
	}
	for _, name := range []string{"a.txt", "sub/b.txt"} {
		got, err := os.ReadFile(rd.SnapshotPath(commit, name))
		if err != nil || len(got) != 1 {
			t.Fatalf("after failed Flatten, snapshot %s: %q, %v", name, got, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(rd.FriendlyPath(), "a.txt")); !os.IsNotExist(err) {
		t.Errorf("a.txt placed although Flatten failed (err=%v)", err)
	}
	if left, _ := filepath.Glob(filepath.Join(rd.FriendlyPath(), "*"+flatStageSuffix)); len(left) > 0 {
		t.Errorf("staged files left behind: %v", left)
	}

	if err := os.Remove(obstacle); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Flatten(FlattenOptions{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(rd.FriendlyPath(), "sub", "b.txt")); string(got) != "B" {
		t.Errorf("sub/b.txt = %q", got)
	}
}

// Copilot review: a repo with its own root hfd.yaml would lose it to the
// manifest when flat, so both ways of flattening refuse it untouched. The
// normal layout keeps it in the cache instead of writing the manifest
// through the friendly link into the cached file.
func TestFlatten_RepoWithOwnManifestFile(t *testing.T) {
	root := t.TempDir()
	rd, _ := NewHFCache(root, 0).Repo("o/r", RepoTypeModel)
	commit := strings.Repeat("6", 40)
	makeSnapshot(t, rd, commit, map[string]string{ManifestFilename: "repo data", "a.txt": "A"})
	if _, err := rd.Flatten(FlattenOptions{}); err == nil || !strings.Contains(err.Error(), ManifestFilename) {
		t.Fatalf("Flatten: %v, want an hfd.yaml conflict", err)
	}
	if _, err := os.Stat(rd.Path()); err != nil {
		t.Fatal("refused Flatten changed the cache")
	}

	hub := &fakeHub{
		commit:        "1111111111111111111111111111111111111111",
		small:         map[string][]byte{ManifestFilename: []byte("repo data"), "a.txt": []byte("A")},
		treeAcceptSHA: true,
		fileAcceptSHA: true,
	}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	root2 := t.TempDir()
	cfg := Settings{Endpoint: srv.URL, CacheDir: root2, Retries: 1, Flatten: true}
	if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err == nil || !strings.Contains(err.Error(), ManifestFilename) {
		t.Fatalf("--flatten Download: %v, want an hfd.yaml conflict", err)
	}
	if hub.gets("/raw/") != 0 {
		t.Errorf("downloaded files before refusing")
	}

	cfg.Flatten = false
	if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err != nil {
		t.Fatal(err)
	}
	rd2, _ := NewHFCache(root2, 0).Repo("o/r", RepoTypeModel)
	if got, _ := os.ReadFile(rd2.SnapshotPath(hub.commit, ManifestFilename)); string(got) != "repo data" {
		t.Errorf("cached hfd.yaml = %q: the manifest was written into the cache", got)
	}
}

// Copilot review: a flat file edited in place (same size) must be restored
// by the next --flatten download; only touching it must not re-download.
func TestDownload_FlattenRestoresEditedFile(t *testing.T) {
	weights := testPayload(300000)
	hub := &fakeHub{
		commit:        "1111111111111111111111111111111111111111",
		files:         map[string][]byte{"model.safetensors": weights},
		treeAcceptSHA: true,
		fileAcceptSHA: true,
	}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	root := t.TempDir()
	cfg := Settings{Endpoint: srv.URL, CacheDir: root, Retries: 1, Flatten: true}
	download := func() {
		t.Helper()
		if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err != nil {
			t.Fatal(err)
		}
	}
	download()
	p := filepath.Join(root, "models", "o", "r", "model.safetensors")
	gets := hub.gets("/model.safetensors")

	// Touched, same content: hashed once, not downloaded.
	later := time.Now().Add(time.Minute)
	testutil.Chtimes(t, p, later, later)
	download()
	if hub.gets("/model.safetensors") != gets {
		t.Fatal("touched but unchanged file was downloaded again")
	}

	// Edited in place, same size: downloaded again and restored.
	edited := append([]byte(nil), weights...)
	edited[0] ^= 0xff
	testutil.WriteFile(t, p, edited, 0o644)
	download()
	if hub.gets("/model.safetensors") == gets {
		t.Fatal("edited file was not downloaded again")
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, weights) {
		t.Fatal("edited file not restored")
	}
}

func TestValidate_FlattenNeedsManifest(t *testing.T) {
	err := Download(context.Background(), Job{Repo: "o/r"}, Settings{Flatten: true, NoManifest: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("Flatten with NoManifest: %v, want an error", err)
	}
}
