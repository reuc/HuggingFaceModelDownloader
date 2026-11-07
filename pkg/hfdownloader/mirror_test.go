// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCopyAndVerifyRepoCache exercises the shared mirror primitives end-to-end:
// CopyRepoCache must reproduce blobs and preserve snapshot symlinks, and
// VerifyRepoCache must pass on a faithful copy but fail on a same-size
// corruption or a missing blob (the SHA256 integrity guarantee).
func TestCopyAndVerifyRepoCache(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	repoRel := "models--owner--name"
	repoPath := filepath.Join(src, repoRel)

	// Build a minimal HF-cache repo: one blob + a snapshot symlink to it.
	blobDir := filepath.Join(repoPath, "blobs")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, "blobA"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapDir := filepath.Join(repoPath, "snapshots", "commit1")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkTarget := filepath.Join("..", "..", "blobs", "blobA")
	if err := os.Symlink(linkTarget, filepath.Join(snapDir, "model.bin")); err != nil {
		t.Fatal(err)
	}

	if err := CopyRepoCache(repoPath, src, dst); err != nil {
		t.Fatalf("CopyRepoCache error: %v", err)
	}

	dstRepo := filepath.Join(dst, repoRel)

	// Blob content copied.
	got, err := os.ReadFile(filepath.Join(dstRepo, "blobs", "blobA"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("copied blob = %q, err=%v; want %q", got, err, "hello")
	}

	// Symlink preserved as a symlink pointing at the same relative target.
	linkPath := filepath.Join(dstRepo, "snapshots", "commit1", "model.bin")
	fi, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("snapshot entry: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("snapshot entry should be a symlink (mode=%v)", fi.Mode())
	}
	if link, _ := os.Readlink(linkPath); link != linkTarget {
		t.Errorf("symlink target = %q, want %q", link, linkTarget)
	}

	// Faithful copy verifies.
	if err := VerifyRepoCache(repoPath, src, dst); err != nil {
		t.Fatalf("VerifyRepoCache on a faithful copy should pass, got %v", err)
	}

	// Same-size corruption must fail the SHA256 check ("world" == len("hello")).
	if err := os.WriteFile(filepath.Join(dstRepo, "blobs", "blobA"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRepoCache(repoPath, src, dst); err == nil {
		t.Error("VerifyRepoCache should fail on same-size corruption")
	}

	// Missing blob must fail.
	if err := os.Remove(filepath.Join(dstRepo, "blobs", "blobA")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRepoCache(repoPath, src, dst); err == nil {
		t.Error("VerifyRepoCache should fail on a missing destination blob")
	}
}
