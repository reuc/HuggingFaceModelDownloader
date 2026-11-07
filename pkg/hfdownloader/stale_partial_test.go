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
	"testing"
)

// A partial of a non-LFS file left by an attempt from before the branch moved
// must never be continued with the new version's bytes: the mix has the right
// size, so a size check alone would keep it. In the HF cache the temp file is
// per version (named by git blob id), so the stale partial isn't used at all;
// in a flat --local-dir the mix fails the git blob id check and is removed, and
// the next run downloads the file cleanly.
func TestDownload_StalePartialOfChangedNonLFSFile(t *testing.T) {
	v1 := []byte(`{"version": 1, "padding": "aaaaaaaaaaaaaaaaaaaa"}`)
	v2 := []byte(`{"version": 2, "padding": "bbbbbbbbbbbbbbbbbbbb"}`)
	if len(v1) != len(v2) {
		t.Fatal("versions must be the same size")
	}
	hub := &fakeHub{commit: "2222222222222222222222222222222222222222",
		small: map[string][]byte{"config.json": v2}, treeAcceptSHA: true, fileAcceptSHA: true}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	job := Job{Repo: "o/r"}

	t.Run("hf cache", func(t *testing.T) {
		cache := t.TempDir()
		blobs := filepath.Join(cache, "hub", "models--o--r", "blobs")
		if err := os.MkdirAll(blobs, 0o755); err != nil {
			t.Fatal(err)
		}
		// Left by an attempt at v1, under the old path-based name and the
		// per-version one.
		for _, it := range []PlanItem{{RelativePath: "config.json"}, {RelativePath: "config.json", GitOID: gitBlobOID(v1)}} {
			if err := os.WriteFile(filepath.Join(blobs, blobTempName(it)+".part"), v1[:20], 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := Download(context.Background(), job, Settings{Endpoint: srv.URL, CacheDir: cache}, nil); err != nil {
			t.Fatalf("Download: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(cache, "models", "o", "r", "config.json"))
		if err != nil || !bytes.Equal(got, v2) {
			t.Fatalf("config.json = %q (err %v), want v2", got, err)
		}
	})

	t.Run("local dir", func(t *testing.T) {
		out := t.TempDir()
		dst := filepath.Join(out, "o", "r", "config.json")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst+".part", v1[:20], 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := Settings{Endpoint: srv.URL, OutputDir: out}
		err := Download(context.Background(), job, cfg, nil)
		var ve *VerificationError
		if !errors.As(err, &ve) || ve.Method != "git blob id" {
			t.Fatalf("Download with a stale partial: err = %v, want a git blob id verification error", err)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Fatalf("the mixed-version file was kept (stat err %v)", err)
		}
		if err := Download(context.Background(), job, cfg, nil); err != nil {
			t.Fatalf("second Download: %v", err)
		}
		if got, _ := os.ReadFile(dst); !bytes.Equal(got, v2) {
			t.Fatalf("config.json = %q, want v2", got)
		}
	})
}
