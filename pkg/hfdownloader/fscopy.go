// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"io"
	"os"
	"path/filepath"
)

// CopyFileStream copies src to dst by streaming through a fixed buffer, so a
// multi-gigabyte blob is never loaded fully into memory (the previous
// os.ReadFile + os.WriteFile approach allocated the whole file and could OOM on
// large model weights). The destination's parent directory is created and the
// source file mode is preserved. The write is atomic-ish: data is written to a
// temp file in the destination directory and renamed into place, so an
// interrupted copy never leaves a half-written blob at the final path.
func CopyFileStream(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".mirror-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename. On success tmp is
	// already closed (its Close error checked below) and renamed away, so
	// both calls are no-ops.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, info.Mode()); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// SameFileSHA256 reports whether two files have byte-identical content by
// comparing their SHA256 digests. Used by mirror --verify to confirm a copied
// blob truly matches the source rather than merely having the same size.
func SameFileSHA256(a, b string) (bool, error) {
	sumA, err := computeSHA256(a)
	if err != nil {
		return false, err
	}
	sumB, err := computeSHA256(b)
	if err != nil {
		return false, err
	}
	return sumA == sumB, nil
}
