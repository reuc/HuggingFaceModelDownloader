// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bodaay/HuggingFaceModelDownloader/pkg/hfdownloader"
)

// writeFlatRepo creates a flat (--flatten) repo: real files plus a
// "storage: flat" manifest, no hub/ entry.
func writeFlatRepo(t *testing.T, cacheDir, kind, repo string) string {
	t.Helper()
	dir := filepath.Join(cacheDir, kind, filepath.FromSlash(repo))
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "w.bin"), make([]byte, 10), 0o644)
	typ := "model"
	if kind == "datasets" {
		typ = "dataset"
	}
	manifest := "version: \"1.0\"\ntype: " + typ + "\nrepo: " + repo + "\nbranch: main\ncommit: 1111111111111111111111111111111111111111\nstorage: flat\nrepo_path: \"\"\ntotal_size: 12\ntotal_files: 2\nfiles:\n" +
		"  - {name: config.json, blob: blobs/" + zeros(64) + ", size: 2, lfs: false}\n" +
		"  - {name: sub/w.bin, blob: blobs/" + zeros(64) + ", size: 10, lfs: false}\n"
	os.WriteFile(filepath.Join(dir, hfdownloader.ManifestFilename), []byte(manifest), 0o644)
	return dir
}

func zeros(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

// Copilot review: naming an already-flat dataset without --dataset is the
// documented no-op, not a "not in the cache" failure.
func TestFlattenCmd_AlreadyFlatDatasetWithoutFlag(t *testing.T) {
	cacheDir := t.TempDir()
	writeFlatRepo(t, cacheDir, "datasets", "o/ds")
	cmd := newFlattenCmd(&RootOpts{Quiet: true})
	cmd.SetArgs([]string{"o/ds", "--cache-dir", cacheDir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("flatten o/ds: %v", err)
	}
}

// Copilot review: info on a flat repo shows its real files, not paths under
// a hub/ entry that no longer exists.
func TestFindRepoInfo_FlatRepo(t *testing.T) {
	cacheDir := t.TempDir()
	dir := writeFlatRepo(t, cacheDir, "models", "o/m")
	info, err := findRepoInfo(cacheDir, "o/m")
	if err != nil {
		t.Fatal(err)
	}
	if info.Storage != hfdownloader.StorageFlat || info.CachePath != "" {
		t.Errorf("storage=%q cache_path=%q, want flat and empty", info.Storage, info.CachePath)
	}
	want := map[string]string{"config.json": filepath.Join(dir, "config.json"), "sub/w.bin": filepath.Join(dir, "sub", "w.bin")}
	for _, f := range info.Files {
		if f.BlobPath != want[f.Name] {
			t.Errorf("%s: path %q, want %q", f.Name, f.BlobPath, want[f.Name])
		}
		if _, err := os.Stat(f.BlobPath); err != nil {
			t.Errorf("%s: %v", f.Name, err)
		}
	}
}
