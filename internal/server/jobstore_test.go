// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// persistConfig returns a config whose job list is saved under a temp dir.
func persistConfig(t *testing.T) Config {
	cfg := DefaultConfig()
	cfg.CacheDir = t.TempDir()
	cfg.JobsFile = filepath.Join(t.TempDir(), "hfdownloader", "jobs.json")
	return cfg
}

// TestJobsSurviveRestart simulates a container restart: one job is running,
// two are queued and one is paused when the process dies. A new manager on the
// same jobs file must restore all of them, queue the running job first and the
// rest in their original order, and leave the paused job paused.
func TestJobsSurviveRestart(t *testing.T) {
	cfg := persistConfig(t)

	m1 := NewJobManager(cfg, nil)
	block := make(chan struct{})
	m1.runFn = func(j *Job) {
		m1.mu.Lock()
		j.Status = JobStatusRunning
		m1.mu.Unlock()
		m1.saveJobs()
		<-block
	}
	var ids []string
	for _, repo := range []string{"o/running", "o/q1", "o/q2", "o/paused"} {
		j, _, err := m1.CreateJob(DownloadRequest{Repo: repo})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		ids = append(ids, j.ID)
	}
	// Make the last one paused, as if the user paused it earlier.
	m1.mu.Lock()
	m1.jobs[ids[3]].Status = JobStatusPaused
	m1.mu.Unlock()
	m1.saveJobs()
	// m1 is now "killed": its runner never returns, nothing else is saved.
	// Unblocking it at the end makes it finish and save once more; wait for
	// that, or the save races the removal of the temp dir.
	t.Cleanup(func() {
		close(block)
		m1.WaitAll(2 * time.Second)
	})

	m2 := NewJobManager(cfg, nil)
	var order []string
	done := make(chan struct{}, 8)
	m2.runFn = func(j *Job) {
		order = append(order, j.Repo)
		done <- struct{}{}
	}

	if got := len(m2.ListJobs()); got != 4 {
		t.Fatalf("restored %d jobs, want 4", got)
	}
	if j, _ := m2.GetJob(ids[3]); j.Status != JobStatusPaused {
		t.Errorf("paused job restored as %q, want paused", j.Status)
	}
	for _, id := range ids[:3] {
		if j, _ := m2.GetJob(id); j.Status != JobStatusQueued {
			t.Errorf("job %s restored as %q, want queued", j.Repo, j.Status)
		}
	}

	m2.Start()
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d restored jobs ran", i)
		}
	}
	m2.WaitAll(2 * time.Second)
	want := []string{"o/running", "o/q1", "o/q2"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("run order = %v, want %v", order, want)
		}
	}
}

// TestJobsFileTracksDismiss checks that removing a job is persisted too, so a
// dismissed job doesn't reappear after a restart (github issue #68).
func TestJobsFileTracksDismiss(t *testing.T) {
	cfg := persistConfig(t)
	m := NewJobManager(cfg, nil)
	m.runFn = func(*Job) {}

	j, _, err := m.CreateJob(DownloadRequest{Repo: "o/r"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	m.WaitAll(2 * time.Second)
	// The fake runner doesn't set a final status; mark it finished.
	m.mu.Lock()
	m.jobs[j.ID].Status = JobStatusCompleted
	m.mu.Unlock()
	if !m.DismissJob(j.ID) {
		t.Fatal("DismissJob failed")
	}

	if got := len(NewJobManager(cfg, nil).ListJobs()); got != 0 {
		t.Fatalf("restored %d jobs after dismiss, want 0", got)
	}
}

// TestJobsFileCorrupt checks that an unreadable jobs file is moved aside
// rather than silently overwritten, and the server still starts empty.
func TestJobsFileCorrupt(t *testing.T) {
	cfg := persistConfig(t)
	if err := os.MkdirAll(filepath.Dir(cfg.JobsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.JobsFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewJobManager(cfg, nil)
	if got := len(m.ListJobs()); got != 0 {
		t.Fatalf("got %d jobs from corrupt file, want 0", got)
	}
	if _, err := os.Stat(cfg.JobsFile + ".bad"); err != nil {
		t.Fatalf("corrupt file not moved aside: %v", err)
	}
}

// TestJobsFileDisabled checks that an empty JobsFile writes nothing.
func TestJobsFileDisabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := DefaultConfig()
	cfg.CacheDir = t.TempDir()
	m := NewJobManager(cfg, nil)
	m.runFn = func(*Job) {}
	if _, _, err := m.CreateJob(DownloadRequest{Repo: "o/r"}); err != nil {
		t.Fatal(err)
	}
	m.WaitAll(2 * time.Second)
	if _, err := os.Stat(DefaultJobsFile()); !os.IsNotExist(err) {
		t.Fatalf("jobs file written with persistence disabled (stat err: %v)", err)
	}
}

// TestWriteFileDurableOverwrites checks that a second save replaces an
// existing jobs file (the case a code review flagged for Windows) and leaves
// no temp file behind.
func TestWriteFileDurableOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := writeJobsFile(path, &jobsState{Version: jobsFileVersion, Queue: []string{"a"}}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := writeJobsFile(path, &jobsState{Version: jobsFileVersion, Queue: []string{"b"}}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got jobsState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Queue) != 1 || got.Queue[0] != "b" {
		t.Fatalf("queue = %v, want [b]", got.Queue)
	}
	if left, _ := filepath.Glob(path + ".*.tmp"); len(left) > 0 {
		t.Fatalf("temp file left behind: %v", left)
	}
}
