// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The commit returned by the Hub's revision API and the sha256/oid in the
// tree listing become snapshots/<commit> and blobs/<sha> paths. They were
// never validated, so a malicious or MITM'd endpoint could place files and
// symlinks (and, via placeLink, delete what was there) anywhere the user can
// write. These tests guard the validation at the plan boundary and the
// defense-in-depth checks in the cache layer.

func TestValidSHA256Hex(t *testing.T) {
	ok := strings.Repeat("ab", 32)
	for _, s := range []string{ok, strings.ToUpper(ok)} {
		if !validSHA256Hex(s) {
			t.Errorf("validSHA256Hex(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"", "abc", strings.Repeat("a", 63), strings.Repeat("a", 65),
		"/../../../../tmp/x" + strings.Repeat("a", 46),
		strings.Repeat("g", 64),
		"..", "../" + strings.Repeat("a", 61),
	} {
		if validSHA256Hex(s) {
			t.Errorf("validSHA256Hex(%q) = true, want false", s)
		}
	}
}

func TestValidSnapshotName(t *testing.T) {
	for _, s := range []string{"0123456789abcdef0123456789abcdef01234567", "main", "v1.0", "release-2"} {
		if !validSnapshotName(s) {
			t.Errorf("validSnapshotName(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "..", "/abs", "/../../home", "a/b", "refs/pr/1", "a\\b", "x\x00y", ".."} {
		if validSnapshotName(s) {
			t.Errorf("validSnapshotName(%q) = true, want false", s)
		}
	}
}

// evilHub serves a repo whose revision and tree responses carry traversal
// strings where a commit and a sha256 are expected.
func evilHub(commit, sha string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/o/r/revision/"):
			json.NewEncoder(w).Encode(map[string]string{"sha": commit})
		case strings.HasPrefix(r.URL.Path, "/api/models/o/r/tree/"):
			json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "model.bin", Size: 4,
				LFS: &hfLfsInfo{Oid: sha, Size: 4}}})
		default:
			w.Write([]byte("data"))
		}
	}))
}

func TestPlanRepo_RejectsUnsafeCommitAndHash(t *testing.T) {
	goodSHA := strings.Repeat("ab", 32)
	for _, tc := range []struct{ name, commit, sha, want string }{
		{"traversal commit", "/../../../../escape", goodSHA, "unsafe commit"},
		{"nested commit", "a/b", goodSHA, "unsafe commit"},
		{"traversal sha", "0123456789abcdef0123456789abcdef01234567", "/../../../../escape", "unsafe sha256"},
		{"short sha", "0123456789abcdef0123456789abcdef01234567", "abc", "unsafe sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := evilHub(tc.commit, tc.sha)
			defer srv.Close()
			_, err := PlanRepo(context.Background(), Job{Repo: "o/r"}, Settings{Endpoint: srv.URL, Retries: 1})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PlanRepo error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestDownload_UnsafeCommitWritesNothing(t *testing.T) {
	srv := evilHub("/../../../../escape", strings.Repeat("ab", 32))
	defer srv.Close()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	err := Download(context.Background(), Job{Repo: "o/r"}, Settings{Endpoint: srv.URL, CacheDir: cacheDir, Retries: 1}, nil)
	if err == nil {
		t.Fatal("Download succeeded with a traversal commit")
	}
	// Nothing may have been created beside the cache directory.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.Name() != "cache" {
			t.Errorf("unexpected entry %q created next to the cache", e.Name())
		}
	}
}

func TestCacheLayer_RejectsUnsafeNames(t *testing.T) {
	cache := NewHFCache(t.TempDir(), 0)
	rd, err := cache.Repo("o/r", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	goodSHA := strings.Repeat("ab", 32)
	if _, _, err := rd.CheckBlob("/../../x"); err == nil {
		t.Error("CheckBlob accepted a traversal hash")
	}
	if err := rd.createSnapshotSymlink("/../../escape", "f", goodSHA); err == nil {
		t.Error("createSnapshotSymlink accepted a traversal commit")
	}
	if err := rd.createSnapshotSymlink("main", "f", "/../../x"); err == nil {
		t.Error("createSnapshotSymlink accepted a traversal hash")
	}
	if err := rd.createSnapshotSymlink("main", "../f", goodSHA); err == nil {
		t.Error("createSnapshotSymlink accepted a traversal path")
	}
	if err := rd.CreateFriendlySymlink("main", "f", "../../out"); err == nil {
		t.Error("CreateFriendlySymlink accepted a traversal filter subdir")
	}
	tmp := filepath.Join(t.TempDir(), "blob")
	os.WriteFile(tmp, []byte("x"), 0o644)
	if _, err := rd.StoreDownloadedFile(tmp, "f", "main", "/../../x", "", true); err == nil {
		t.Error("StoreDownloadedFile accepted a traversal hash")
	}
}

// The generated rebuild.sh used to splice file paths into a python -c
// program; a repo file name containing a quote ran arbitrary code.
func TestRebuildScript_PassesPathsAsArguments(t *testing.T) {
	if strings.Contains(RebuildScript, "relpath('$") {
		t.Fatal("rebuild.sh interpolates shell variables into Python source")
	}
	if !strings.Contains(RebuildScript, `sys.argv[1], sys.argv[2]))' "$target_path" "$friendly_parent"`) {
		t.Fatal("rebuild.sh should pass paths to Python as arguments")
	}
}
