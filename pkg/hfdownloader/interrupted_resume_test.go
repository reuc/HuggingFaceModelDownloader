// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// cuttingHub is a fakeHub whose file responses can be cut off after a number
// of body bytes (a dropped connection); it records each file GET's Range.
type cuttingHub struct {
	*fakeHub
	mu     sync.Mutex
	cut    int
	ranges []string
}

func (c *cuttingHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/resolve/") {
		c.mu.Lock()
		c.ranges = append(c.ranges, r.Header.Get("Range"))
		n := c.cut
		c.mu.Unlock()
		if n > 0 {
			w = &cutResponse{ResponseWriter: w, left: n}
		}
	}
	c.fakeHub.ServeHTTP(w, r)
}

func (c *cuttingHub) set(n int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cut = n
	r := c.ranges
	c.ranges = nil
	return r
}

type cutResponse struct {
	http.ResponseWriter
	left int
}

func (c *cutResponse) Write(p []byte) (int, error) {
	if len(p) <= c.left {
		c.left -= len(p)
		return c.ResponseWriter.Write(p)
	}
	c.ResponseWriter.Write(p[:c.left])
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	panic(http.ErrAbortHandler)
}

func blobTempPath(cache string, data []byte) string {
	sum := sha256.Sum256(data)
	return filepath.Join(cache, "hub", "models--o--r", "blobs", "tmp-"+hex.EncodeToString(sum[:]))
}

// A download stopped while verifying a finished file (pause in the web UI,
// Ctrl+C in the CLI) used to leave the complete file under its temp name,
// which the next run ignored and downloaded again from zero. It now checks
// the finished file and stores it without downloading anything.
func TestDownload_StoppedWhileVerifyingIsNotDownloadedAgain(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Settings
	}{
		{"single stream", Settings{MultipartThreshold: "1GiB"}},
		{"multipart", Settings{MultipartThreshold: "1KiB", Concurrency: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			weights := testPayload(3 << 20) // hashed in several 1 MiB chunks
			hub := &fakeHub{commit: "3333333333333333333333333333333333333333",
				files: map[string][]byte{"model.safetensors": weights}, treeAcceptSHA: true, fileAcceptSHA: true}
			srv := httptest.NewServer(hub)
			defer srv.Close()
			cfg := tc.cfg
			cfg.Endpoint, cfg.CacheDir = srv.URL, t.TempDir()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := Download(ctx, Job{Repo: "o/r"}, cfg, func(ev ProgressEvent) {
				if ev.Event == "file_verify" {
					cancel() // stop as soon as verification starts
				}
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("first run: err = %v, want it stopped during verification", err)
			}
			if fi, err := os.Stat(blobTempPath(cfg.CacheDir, weights)); err != nil || fi.Size() != int64(len(weights)) {
				t.Fatalf("finished temp file after the stop: %v", err)
			}
			gets := hub.gets("/model.safetensors")

			var events []string
			if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, func(ev ProgressEvent) { events = append(events, ev.Event) }); err != nil {
				t.Fatalf("second run: %v", err)
			}
			if n := hub.gets("/model.safetensors") - gets; n != 0 {
				t.Errorf("second run downloaded the finished file again (%d GETs)", n)
			}
			if !strings.Contains(strings.Join(events, " "), "file_verify") {
				t.Error("second run stored the file without verifying it")
			}
			if got, _ := os.ReadFile(filepath.Join(cfg.CacheDir, "models", "o", "r", "model.safetensors")); !bytes.Equal(got, weights) {
				t.Fatal("stored file is wrong")
			}
		})
	}
}

// A finished temp file that doesn't verify is removed and downloaded again in
// the same run, rather than failing it.
func TestDownload_CorruptFinishedTempFileIsDownloadedAgain(t *testing.T) {
	weights := testPayload(200000)
	hub := &fakeHub{commit: "4444444444444444444444444444444444444444",
		files: map[string][]byte{"model.safetensors": weights}, treeAcceptSHA: true, fileAcceptSHA: true}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	cache := t.TempDir()

	bad := append([]byte(nil), weights...)
	bad[1234] ^= 0xff
	tmp := blobTempPath(cache, weights)
	os.MkdirAll(filepath.Dir(tmp), 0o755)
	if err := os.WriteFile(tmp, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Download(context.Background(), Job{Repo: "o/r"}, Settings{Endpoint: srv.URL, CacheDir: cache}, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if hub.gets("/model.safetensors") == 0 {
		t.Error("the corrupt file was not downloaded again")
	}
	if got, _ := os.ReadFile(filepath.Join(cache, "models", "o", "r", "model.safetensors")); !bytes.Equal(got, weights) {
		t.Fatal("stored file is wrong")
	}
}

// Changing the connection count between attempts (web UI settings, `resume
// -c`) used to discard every part of a partly downloaded multipart file. The
// file now keeps the split it was started with and each part continues.
func TestDownloadMultipart_KeepsItsSplitWhenConnectionsChange(t *testing.T) {
	const parts, partSize = 4, 100000
	weights := testPayload(parts * partSize)
	hub := &cuttingHub{fakeHub: &fakeHub{commit: "5555555555555555555555555555555555555555",
		files: map[string][]byte{"model.safetensors": weights}, treeAcceptSHA: true, fileAcceptSHA: true}}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	cache := t.TempDir()
	cfg := Settings{Endpoint: srv.URL, CacheDir: cache, MultipartThreshold: "1KiB", Concurrency: parts}

	hub.set(30000)
	if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err == nil {
		t.Fatal("first run succeeded despite dropped connections")
	}
	tmp := blobTempPath(cache, weights)
	var want []string
	for i := 0; i < parts; i++ {
		fi, err := os.Stat(fmt.Sprintf("%s.part-%02d", tmp, i))
		if err != nil || fi.Size() == 0 {
			t.Fatalf("part %d after the first run: %v", i, err)
		}
		want = append(want, fmt.Sprintf("bytes=%d-%d", i*partSize+int(fi.Size()), (i+1)*partSize-1))
	}

	hub.set(0)
	cfg.Concurrency = 2
	if err := Download(context.Background(), Job{Repo: "o/r"}, cfg, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	got := hub.set(0)
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("second run did not continue a part with %q (requests: %v)", w, got)
		}
	}
	if stored, _ := os.ReadFile(filepath.Join(cache, "models", "o", "r", "model.safetensors")); !bytes.Equal(stored, weights) {
		t.Fatal("stored file is wrong")
	}
}
