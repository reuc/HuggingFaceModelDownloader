// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reuc/HuggingFaceModelDownloader/internal/testutil"
)

// hubRepo writes a minimal HF cache entry (one config.json per commit, refs/main
// at the last commit) for repo under hub/<kind>--owner--name.
func hubRepo(t *testing.T, cacheDir, kind, repo string, commits ...string) string {
	t.Helper()
	dir := filepath.Join(cacheDir, "hub", kind+"--"+strings.ReplaceAll(repo, "/", "--"))
	testutil.MkdirAll(t, filepath.Join(dir, "blobs"), 0o755)
	for i, c := range commits {
		snap := filepath.Join(dir, "snapshots", c)
		testutil.MkdirAll(t, snap, 0o755)
		blob := fmt.Sprintf("%064d", i+1)
		testutil.WriteFile(t, filepath.Join(dir, "blobs", blob), []byte(repo+"@"+c), 0o644)
		if err := os.Symlink(filepath.Join("..", "..", "blobs", blob), filepath.Join(snap, "config.json")); err != nil {
			t.Fatal(err)
		}
	}
	testutil.MkdirAll(t, filepath.Join(dir, "refs"), 0o755)
	testutil.WriteFile(t, filepath.Join(dir, "refs", "main"), []byte(commits[len(commits)-1]), 0o644)
	return dir
}

func TestConvertOnBoot(t *testing.T) {
	c1, c2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	cacheDir := t.TempDir()
	plain := hubRepo(t, cacheDir, "models", "a/plain", c1)
	dataset := hubRepo(t, cacheDir, "datasets", "d/ds", c1)
	multi := hubRepo(t, cacheDir, "models", "a/multi", c1, c2)
	queued := hubRepo(t, cacheDir, "models", "a/queued", c1)
	paused := hubRepo(t, cacheDir, "models", "a/paused", c1)

	srv := New(Config{CacheDir: cacheDir, Flatten: true, ConvertOnBoot: true})
	srv.jobs.mu.Lock()
	srv.jobs.jobs["q"] = &Job{ID: "q", Repo: "a/queued", Status: JobStatusQueued}
	srv.jobs.jobs["p"] = &Job{ID: "p", Repo: "a/paused", Status: JobStatusPaused}
	srv.jobs.mu.Unlock()

	sum := srv.convertOnBoot(context.Background())
	if sum != (convertSummary{Converted: 2, Skipped: 3}) {
		t.Fatalf("summary = %+v, want 2 converted, 3 skipped", sum)
	}

	// Converted: real files in models/ or datasets/, no hub/ entry.
	for _, c := range []struct{ hub, file, want string }{
		{plain, filepath.Join(cacheDir, "models", "a", "plain", "config.json"), "a/plain@" + c1},
		{dataset, filepath.Join(cacheDir, "datasets", "d", "ds", "config.json"), "d/ds@" + c1},
	} {
		if _, err := os.Stat(c.hub); !os.IsNotExist(err) {
			t.Errorf("%s still exists", c.hub)
		}
		fi, err := os.Lstat(c.file)
		if err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("%s: not a real file (%v)", c.file, err)
		}
		if got, _ := os.ReadFile(c.file); string(got) != c.want {
			t.Errorf("%s = %q, want %q", c.file, got, c.want)
		}
	}
	// Skipped: left exactly as they were.
	for _, dir := range []string{multi, queued, paused} {
		if _, err := os.Stat(filepath.Join(dir, "refs", "main")); err != nil {
			t.Errorf("skipped repo %s was changed: %v", dir, err)
		}
	}

	// The next boot finds nothing new to convert.
	if sum := srv.convertOnBoot(context.Background()); sum.Converted != 0 || sum.Failed != 0 {
		t.Errorf("second boot = %+v, want nothing converted", sum)
	}
}

func TestConvertOnBoot_LocalDirMode(t *testing.T) {
	cacheDir := t.TempDir()
	plain := hubRepo(t, cacheDir, "models", "a/plain", strings.Repeat("1", 40))
	srv := New(Config{CacheDir: cacheDir, LocalDir: t.TempDir(), ConvertOnBoot: true})
	if sum := srv.convertOnBoot(context.Background()); sum != (convertSummary{}) {
		t.Fatalf("summary = %+v, want nothing done in --local-dir mode", sum)
	}
	if _, err := os.Stat(plain); err != nil {
		t.Fatal("hub/ repo changed in --local-dir mode")
	}
}

// Copilot review: a shutdown during the startup conversion stops it before
// the next repo instead of converting everything first.
func TestConvertOnBoot_StopsOnShutdown(t *testing.T) {
	cacheDir := t.TempDir()
	a := hubRepo(t, cacheDir, "models", "a/one", strings.Repeat("1", 40))
	b := hubRepo(t, cacheDir, "models", "a/two", strings.Repeat("1", 40))
	srv := New(Config{CacheDir: cacheDir, Flatten: true, ConvertOnBoot: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sum := srv.convertOnBoot(ctx); sum != (convertSummary{Remaining: 2}) {
		t.Fatalf("summary = %+v, want both left for the next start", sum)
	}
	for _, dir := range []string{a, b} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s changed after shutdown: %v", dir, err)
		}
	}
}

// Copilot review: the logged recovery command must be one the CLI accepts.
func TestFlattenCommand(t *testing.T) {
	cmds := map[string]string{
		flattenCommand("o/m", false): "hfdownloader flatten --yes o/m",
		flattenCommand("o/d", true):  "hfdownloader flatten --yes --dataset o/d",
	}
	for got, want := range cmds {
		if got != want {
			t.Errorf("flattenCommand = %q, want %q", got, want)
		}
	}
}
