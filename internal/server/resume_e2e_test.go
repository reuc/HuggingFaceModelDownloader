// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const e2eCommit = "6666666666666666666666666666666666666666"

// e2eHub is a minimal Hub serving LFS files of repo o/r. Responses can be made
// to stop partway: hang sends n body bytes and then stalls until the client
// gives up (the download is mid-file when it is paused); cut drops the
// connection after n bytes (a network failure).
type e2eHub struct {
	files map[string][]byte

	mu      sync.Mutex
	hang    int
	cut     int
	started chan struct{}
	ranges  []string // "<path> <Range>" of each file GET
}

func newE2EHub(files map[string][]byte) *e2eHub {
	return &e2eHub{files: files, started: make(chan struct{}, 64)}
}

func (h *e2eHub) set(hang, cut int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hang, h.cut = hang, cut
	r := h.ranges
	h.ranges = nil
	return r
}

func (h *e2eHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/models/o/r/revision/"):
		json.NewEncoder(w).Encode(map[string]string{"sha": e2eCommit})
	case strings.HasPrefix(p, "/api/models/o/r/tree/"):
		var nodes []map[string]any
		for path, data := range h.files {
			sum := sha256.Sum256(data)
			nodes = append(nodes, map[string]any{"type": "file", "path": path, "size": 134,
				"lfs": map[string]any{"oid": hex.EncodeToString(sum[:]), "size": len(data)}})
		}
		json.NewEncoder(w).Encode(nodes)
	case strings.HasPrefix(p, "/o/r/resolve/"):
		_, path, _ := strings.Cut(strings.TrimPrefix(p, "/o/r/resolve/"), "/")
		data, ok := h.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var out http.ResponseWriter = w
		if r.Method == http.MethodGet {
			h.mu.Lock()
			h.ranges = append(h.ranges, path+" "+r.Header.Get("Range"))
			hang, cut := h.hang, h.cut
			h.mu.Unlock()
			switch {
			case hang > 0:
				out = &stopWriter{ResponseWriter: w, left: hang, hang: r.Context(), started: h.started}
			case cut > 0:
				out = &stopWriter{ResponseWriter: w, left: cut}
			}
		}
		http.ServeContent(out, r, path, time.Time{}, bytes.NewReader(data))
	default:
		http.NotFound(w, r)
	}
}

type stopWriter struct {
	http.ResponseWriter
	left    int
	hang    context.Context
	started chan struct{}
}

func (s *stopWriter) Write(p []byte) (int, error) {
	if len(p) <= s.left {
		s.left -= len(p)
		return s.ResponseWriter.Write(p)
	}
	s.ResponseWriter.Write(p[:s.left])
	s.left = 0
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	if s.hang == nil {
		panic(http.ErrAbortHandler)
	}
	s.started <- struct{}{}
	<-s.hang.Done()
	return 0, s.hang.Err()
}

func e2ePayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/509)
	}
	return b
}

func e2eTempPath(cache string, data []byte) string {
	sum := sha256.Sum256(data)
	return filepath.Join(cache, "hub", "models--o--r", "blobs", "tmp-"+hex.EncodeToString(sum[:]))
}

// waitStatus polls until the job reaches want, failing on another final state.
func waitStatus(t *testing.T, m *JobManager, id string, want JobStatus) *Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, ok := m.GetJob(id)
		if !ok {
			t.Fatalf("job %s disappeared", id)
		}
		if j.Status == want {
			return j
		}
		if isTerminalJobStatus(j.Status) && j.Status != JobStatusPaused {
			t.Fatalf("job %s is %s (error %q), want %s", id, j.Status, j.Error, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s still %s, want %s", id, j.Status, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFileSizes waits until the files together hold at least n bytes.
func waitFileSizes(t *testing.T, n int64, paths ...string) []int64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var sizes []int64
		var total int64
		for _, p := range paths {
			var sz int64
			if fi, err := os.Stat(p); err == nil {
				sz = fi.Size()
			}
			sizes = append(sizes, sz)
			total += sz
		}
		if total >= n {
			return sizes
		}
		if time.Now().After(deadline) {
			t.Fatalf("partial files hold %d bytes, want %d", total, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func hasAll(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("no request %q (requests: %v)", w, got)
		}
	}
}

// Pause in the middle of a multipart file, change the connection count in
// Settings, Resume: every part continues from its partial data and the job
// completes with the right file and progress.
func TestJobs_PauseResumeContinuesPartialData(t *testing.T) {
	const parts, partSize = 4, 100000
	weights := e2ePayload(parts * partSize)
	hub := newE2EHub(map[string][]byte{"model.safetensors": weights})
	srv := httptest.NewServer(hub)
	defer srv.Close()
	cache := t.TempDir()
	cfg := Config{CacheDir: cache, Endpoint: srv.URL, Concurrency: parts, MaxActive: 2, MultipartThreshold: "1KiB", Verify: "size"}
	m := NewJobManager(cfg, nil)
	defer m.WaitAll(5 * time.Second)

	hub.set(30000, 0)
	job, _, err := m.CreateJob(DownloadRequest{Repo: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	tmp := e2eTempPath(cache, weights)
	var partPaths []string
	for i := 0; i < parts; i++ {
		partPaths = append(partPaths, fmt.Sprintf("%s.part-%02d", tmp, i))
	}
	sizes := waitFileSizes(t, parts*30000, partPaths...)

	if !m.PauseJob(job.ID) {
		t.Fatal("PauseJob refused a running job")
	}
	if !m.WaitAll(5 * time.Second) {
		t.Fatal("paused run did not stop")
	}
	waitStatus(t, m, job.ID, JobStatusPaused)

	cfg.Concurrency = 2 // changed in Settings while paused
	m.UpdateConfig(cfg)
	hub.set(0, 0)
	if !m.ResumeJob(job.ID) {
		t.Fatal("ResumeJob refused a paused job")
	}
	done := waitStatus(t, m, job.ID, JobStatusCompleted)

	var want []string
	for i, sz := range sizes {
		want = append(want, fmt.Sprintf("model.safetensors bytes=%d-%d", int64(i*partSize)+sz, (i+1)*partSize-1))
	}
	hasAll(t, hub.set(0, 0), want...)
	if got, _ := os.ReadFile(filepath.Join(cache, "models", "o", "r", "model.safetensors")); !bytes.Equal(got, weights) {
		t.Fatal("downloaded file is wrong")
	}
	p := done.Progress
	if p.CompletedFiles != 1 || p.TotalFiles != 1 || p.DownloadedBytes != int64(len(weights)) || p.TotalBytes != int64(len(weights)) {
		t.Errorf("final progress = %+v", p)
	}
}

// Redownload of a failed job continues the partial file instead of starting
// over.
func TestJobs_RetryContinuesPartialData(t *testing.T) {
	weights := e2ePayload(200000)
	hub := newE2EHub(map[string][]byte{"model.safetensors": weights})
	srv := httptest.NewServer(hub)
	defer srv.Close()
	cache := t.TempDir()
	m := NewJobManager(Config{CacheDir: cache, Endpoint: srv.URL, MultipartThreshold: "1GiB", Retries: 0, Verify: "size"}, nil)
	defer m.WaitAll(5 * time.Second)

	hub.set(0, 70000)
	job, _, err := m.CreateJob(DownloadRequest{Repo: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, job.ID, JobStatusFailed)
	part := waitFileSizes(t, 1, e2eTempPath(cache, weights)+".part")[0]

	hub.set(0, 0)
	if res, _ := m.RetryJob(job.ID); res != RetryJobOK {
		t.Fatalf("RetryJob = %v", res)
	}
	waitStatus(t, m, job.ID, JobStatusCompleted)
	hasAll(t, hub.set(0, 0), fmt.Sprintf("model.safetensors bytes=%d-%d", part, len(weights)-1))
	if got, _ := os.ReadFile(filepath.Join(cache, "models", "o", "r", "model.safetensors")); !bytes.Equal(got, weights) {
		t.Fatal("downloaded file is wrong")
	}
}

// A job paused before a restart, with the server now started with another
// cache directory, resumes where its data is (and where its card says it
// goes) instead of starting over in the new directory.
func TestJobs_RestoredJobResumesInItsOwnDirectory(t *testing.T) {
	weights := e2ePayload(200000)
	hub := newE2EHub(map[string][]byte{"model.safetensors": weights})
	srv := httptest.NewServer(hub)
	defer srv.Close()
	jobsFile := filepath.Join(t.TempDir(), "jobs.json")
	cacheA, cacheB := t.TempDir(), t.TempDir()
	base := Config{Endpoint: srv.URL, MultipartThreshold: "1GiB", Verify: "size", JobsFile: jobsFile}

	cfgA := base
	cfgA.CacheDir = cacheA
	m1 := NewJobManager(cfgA, nil)
	hub.set(50000, 0)
	job, _, err := m1.CreateJob(DownloadRequest{Repo: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	part := waitFileSizes(t, 50000, e2eTempPath(cacheA, weights)+".part")[0]
	m1.PauseJob(job.ID)
	if !m1.WaitAll(5 * time.Second) {
		t.Fatal("paused run did not stop")
	}

	// Restart with another cache directory.
	cfgB := base
	cfgB.CacheDir = cacheB
	m2 := NewJobManager(cfgB, nil)
	defer m2.WaitAll(5 * time.Second)
	m2.Start()
	restored, ok := m2.GetJob(job.ID)
	if !ok || restored.Status != JobStatusPaused || restored.OutputDir != cacheA {
		t.Fatalf("restored job = %+v", restored)
	}
	hub.set(0, 0)
	if !m2.ResumeJob(job.ID) {
		t.Fatal("ResumeJob refused the restored job")
	}
	waitStatus(t, m2, job.ID, JobStatusCompleted)

	hasAll(t, hub.set(0, 0), fmt.Sprintf("model.safetensors bytes=%d-%d", part, len(weights)-1))
	if got, _ := os.ReadFile(filepath.Join(cacheA, "models", "o", "r", "model.safetensors")); !bytes.Equal(got, weights) {
		t.Fatal("the job did not finish in its own cache directory")
	}
	if _, err := os.Stat(filepath.Join(cacheB, "hub")); !os.IsNotExist(err) {
		t.Errorf("the job started over in the new cache directory (stat err %v)", err)
	}
}

// Speed counts only bytes moved by this run: data a resumed file already had
// and files skipped as complete used to show up as a burst of GB/s.
func TestSpeedMeter_CountsOnlyThisRunsBytes(t *testing.T) {
	const mb = 1 << 20
	var s speedMeter
	t0 := time.Now()
	files := []JobFileProgress{{Path: "a", Downloaded: 900 * mb}} // resumed: 900 MB on disk
	s.seen("a", files[0].Downloaded)
	if _, ok := s.sample(t0, files); ok {
		t.Fatal("first sample produced a speed")
	}

	files[0].Downloaded += 10 * mb
	if bps, ok := s.sample(t0.Add(time.Second), files); !ok || bps != 10*mb {
		t.Fatalf("speed = %d (ok %v), want 10 MB/s", bps, ok)
	}

	// Another resumed file reporting its 500 MB on disk, and a 300 MB file
	// skipped as complete (never seen by progress), move nothing: the
	// average only decays.
	ema := float64(10 * mb)
	ema = 0.3*0 + 0.7*ema
	files = append(files, JobFileProgress{Path: "b", Downloaded: 500 * mb}, JobFileProgress{Path: "c", Downloaded: 300 * mb})
	s.seen("b", 500*mb)
	if bps, _ := s.sample(t0.Add(2*time.Second), files); bps != int64(ema) {
		t.Fatalf("speed after resumed/skipped files = %d, want %d", bps, int64(ema))
	}

	// A file downloaded without any progress report counts in full.
	ema = 0.3*float64(10*mb) + 0.7*ema
	s.seen("d", 0)
	files = append(files, JobFileProgress{Path: "d", Downloaded: 10 * mb})
	if bps, _ := s.sample(t0.Add(3*time.Second), files); bps != int64(ema) {
		t.Fatalf("speed with a small finished file = %d, want %d", bps, int64(ema))
	}
}
