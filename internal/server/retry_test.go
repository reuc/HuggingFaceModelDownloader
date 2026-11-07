// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// failedJob returns a job as runJob leaves it after a download error.
func failedJob(id, repo string) *Job {
	ended := time.Now()
	return &Job{
		ID:        id,
		Repo:      repo,
		Revision:  "main",
		Filters:   []string{"q4_k_m"},
		Status:    JobStatusFailed,
		Error:     "download a.gguf: http2: client connection lost",
		CreatedAt: time.Now().Add(-time.Hour),
		StartedAt: &ended,
		EndedAt:   &ended,
		Progress:  JobProgress{TotalFiles: 2, CompletedFiles: 1, TotalBytes: 10, DownloadedBytes: 6},
		Files:     []JobFileProgress{{Path: "a.gguf", TotalBytes: 10, Downloaded: 6, Status: "active"}},
	}
}

// Redownload puts a failed job back in the queue under the same ID, with the
// error and stale progress cleared, and the scheduler runs it.
func TestRetryJob_RequeuesFailedJob(t *testing.T) {
	m := NewJobManager(Config{}, nil)
	started := make(chan string, 1)
	m.runFn = func(j *Job) { started <- j.ID }
	m.jobs["f1"] = failedJob("f1", "owner/repo")

	res, snap := m.RetryJob("f1")
	if res != RetryJobOK {
		t.Fatalf("RetryJob = %v, want RetryJobOK", res)
	}
	if snap.ID != "f1" || snap.Status != JobStatusQueued {
		t.Errorf("snapshot id=%q status=%q, want f1 queued", snap.ID, snap.Status)
	}
	if snap.Error != "" || snap.EndedAt != nil || snap.StartedAt != nil {
		t.Errorf("error/timestamps not cleared: error=%q started=%v ended=%v", snap.Error, snap.StartedAt, snap.EndedAt)
	}
	if snap.Progress != (JobProgress{}) || snap.Files != nil {
		t.Errorf("stale progress kept: %+v files=%v", snap.Progress, snap.Files)
	}
	if len(snap.Filters) != 1 || snap.Filters[0] != "q4_k_m" {
		t.Errorf("selection changed: filters=%v", snap.Filters)
	}

	select {
	case id := <-started:
		if id != "f1" {
			t.Errorf("scheduler ran %q, want f1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("re-queued job never started")
	}
	if !m.WaitAll(2 * time.Second) {
		t.Fatal("run did not finish")
	}
}

// A retried job waits its turn behind a download that is already running.
func TestRetryJob_QueuesBehindRunningJob(t *testing.T) {
	m := NewJobManager(Config{}, nil)
	release := make(chan struct{})
	started := make(chan string, 2)
	m.runFn = func(j *Job) {
		started <- j.Repo
		<-release
	}
	if _, _, err := m.CreateJob(DownloadRequest{Repo: "owner/busy"}); err != nil {
		t.Fatal(err)
	}
	<-started
	m.mu.Lock()
	m.jobs["f1"] = failedJob("f1", "owner/repo")
	m.mu.Unlock()

	if res, _ := m.RetryJob("f1"); res != RetryJobOK {
		t.Fatalf("RetryJob = %v, want RetryJobOK", res)
	}
	select {
	case repo := <-started:
		t.Fatalf("%s started while another download was running", repo)
	case <-time.After(50 * time.Millisecond):
	}
	if j, _ := m.GetJob("f1"); j.Status != JobStatusQueued {
		t.Errorf("status %q, want queued", j.Status)
	}

	close(release)
	select {
	case repo := <-started:
		if repo != "owner/repo" {
			t.Errorf("next job %q, want owner/repo", repo)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retried job never started after the running one finished")
	}
	if !m.WaitAll(2 * time.Second) {
		t.Fatal("runs did not finish")
	}
}

func TestRetryJob_OnlyFailedJobs(t *testing.T) {
	m := NewJobManager(Config{}, nil)
	m.runFn = func(*Job) {}

	if res, _ := m.RetryJob("missing"); res != RetryJobNotFound {
		t.Errorf("missing job: %v, want RetryJobNotFound", res)
	}
	for _, st := range []JobStatus{JobStatusQueued, JobStatusRunning, JobStatusPaused, JobStatusCompleted, JobStatusCancelled} {
		id := "job-" + string(st)
		m.jobs[id] = &Job{ID: id, Repo: "owner/" + id, Status: st, CreatedAt: time.Now()}
		if res, _ := m.RetryJob(id); res != RetryJobNotFailed {
			t.Errorf("%s: %v, want RetryJobNotFailed", st, res)
		}
		if m.jobs[id].Status != st {
			t.Errorf("%s: status changed to %s", st, m.jobs[id].Status)
		}
	}
}

// If the same download was started again as a new job, retrying the old one
// would download it twice; a different selection of the same repo is fine.
func TestRetryJob_RefusesActiveDuplicate(t *testing.T) {
	m := NewJobManager(Config{}, nil)
	m.runFn = func(*Job) {}
	m.jobs["f1"] = failedJob("f1", "owner/repo")
	m.jobs["dup"] = &Job{ID: "dup", Repo: "owner/repo", Revision: "main", Filters: []string{"q4_k_m"},
		Status: JobStatusRunning, CreatedAt: time.Now()}

	res, other := m.RetryJob("f1")
	if res != RetryJobDuplicate || other == nil || other.ID != "dup" {
		t.Fatalf("RetryJob = %v (%v), want RetryJobDuplicate of dup", res, other)
	}
	if m.jobs["f1"].Status != JobStatusFailed {
		t.Errorf("status %q, want it to stay failed", m.jobs["f1"].Status)
	}

	m.jobs["dup"].Filters = []string{"q8_0"}
	if res, _ := m.RetryJob("f1"); res != RetryJobOK {
		t.Errorf("different filters: %v, want RetryJobOK", res)
	}
	m.WaitAll(2 * time.Second)
}

func TestAPI_RetryJob(t *testing.T) {
	srv := newTestServer()
	release := make(chan struct{})
	t.Cleanup(func() { close(release); srv.jobs.WaitAll(2 * time.Second) })
	srv.jobs.runFn = func(*Job) { <-release }
	srv.jobs.jobs["f1"] = failedJob("f1", "owner/repo")
	srv.jobs.jobs["done"] = &Job{ID: "done", Repo: "owner/done", Status: JobStatusCompleted, CreatedAt: time.Now()}
	mux := http.NewServeMux()
	srv.registerAPIRoutes(mux)

	post := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		return w
	}

	w := post("/api/jobs/f1/retry")
	if w.Code != http.StatusOK {
		t.Fatalf("retry failed job: status %d, body %s", w.Code, w.Body)
	}
	var got Job
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "f1" || got.Status != JobStatusQueued || got.Error != "" {
		t.Errorf("response job id=%q status=%q error=%q", got.ID, got.Status, got.Error)
	}

	if w := post("/api/jobs/f1/retry"); w.Code != http.StatusConflict {
		t.Errorf("retry an active job: status %d, want 409", w.Code)
	}
	if w := post("/api/jobs/done/retry"); w.Code != http.StatusConflict {
		t.Errorf("retry a completed job: status %d, want 409", w.Code)
	}
	if w := post("/api/jobs/nope/retry"); w.Code != http.StatusNotFound {
		t.Errorf("retry a missing job: status %d, want 404", w.Code)
	}
}
