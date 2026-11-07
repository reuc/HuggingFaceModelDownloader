// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
)

// jobsFileVersion is bumped if the on-disk job list format changes
// incompatibly; files with another version are ignored (and set aside).
const jobsFileVersion = 1

// jobsState is the on-disk format of the job list. Queue records the FIFO
// order of jobs waiting to run so a restart keeps it; a job that was running
// at shutdown is restored to the front.
type jobsState struct {
	Version int      `json:"version"`
	Queue   []string `json:"queue,omitempty"`
	Jobs    []*Job   `json:"jobs"`
}

// DefaultJobsFile returns where `serve` keeps its job list by default:
// ~/.config/hfdownloader/jobs.json, next to the mirror targets file.
func DefaultJobsFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hfdownloader", "jobs.json")
}

// saveJobs writes the current job list to m.jobsFile (no-op when unset). It
// must be called WITHOUT m.mu held. saveMu serializes writers and is held
// across snapshot+write, so a later call always writes a state at least as
// new as an earlier one. Called on status changes only, never on progress.
func (m *JobManager) saveJobs() {
	if m.jobsFile == "" {
		return
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()

	m.mu.RLock()
	state := jobsState{Version: jobsFileVersion, Jobs: make([]*Job, 0, len(m.jobs))}
	for _, j := range m.jobs {
		clone := m.cloneJobLocked(j)
		// Per-file progress is rebuilt by the next run's plan and can be
		// thousands of entries for big datasets; don't persist it.
		clone.Files = nil
		state.Jobs = append(state.Jobs, clone)
	}
	for _, id := range m.queue {
		if j, ok := m.jobs[id]; ok && j.Status == JobStatusQueued {
			state.Queue = append(state.Queue, id)
		}
	}
	m.mu.RUnlock()

	sort.Slice(state.Jobs, func(a, b int) bool {
		return state.Jobs[a].CreatedAt.Before(state.Jobs[b].CreatedAt)
	})

	if err := writeJobsFile(m.jobsFile, &state); err != nil {
		log.Printf("warning: could not save job list to %s: %v", m.jobsFile, err)
	}
}

func writeJobsFile(path string, state *jobsState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileDurable(path, data, 0o600)
}

// writeFileDurable replaces path with data so that after a crash or power loss
// the file holds either the old or the new contents, never a truncated or
// empty one. The data is written to a temp file in the same directory, flushed
// to disk, then renamed over path (os.Rename replaces an existing file on all
// platforms, including Windows). Finally the directory is synced so the rename
// itself survives a crash; that step is best effort because Windows can't
// fsync a directory. Each call uses its own temp file, so two writers (e.g.
// two server processes sharing a config dir) can't rename each other's.
func writeFileDurable(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// loadJobs restores the job list saved by a previous server process. Jobs that
// were queued or running are put back in the queue (running first, then the
// saved queue order) so they continue when the scheduler starts; partially
// downloaded files are resumed by the downloader. Paused jobs stay paused and
// finished jobs are kept as history until dismissed. A missing file is not an
// error; an unreadable one is moved aside so it isn't overwritten.
func (m *JobManager) loadJobs() error {
	if m.jobsFile == "" {
		return nil
	}
	data, err := os.ReadFile(m.jobsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var state jobsState
	if err := json.Unmarshal(data, &state); err != nil || state.Version != jobsFileVersion {
		bad := m.jobsFile + ".bad"
		_ = os.Rename(m.jobsFile, bad)
		if err == nil {
			err = fmt.Errorf("unsupported version %d", state.Version)
		}
		return fmt.Errorf("unreadable job list (moved to %s): %w", bad, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var running []*Job
	for _, j := range state.Jobs {
		if j == nil || j.ID == "" || j.Repo == "" {
			continue
		}
		if _, dup := m.jobs[j.ID]; dup {
			continue
		}
		j.Files = nil
		j.Progress.Activity = ""
		j.Progress.BytesPerSecond = 0
		switch j.Status {
		case JobStatusRunning, JobStatusQueued:
			if j.Status == JobStatusRunning {
				running = append(running, j)
			}
			j.Status = JobStatusQueued
			j.Error = ""
			// The downloader re-plans and reports every file again,
			// skipping the ones already complete (same as ResumeJob).
			j.Progress = JobProgress{}
		case JobStatusPaused, JobStatusCompleted, JobStatusFailed, JobStatusCancelled:
		default:
			continue
		}
		m.jobs[j.ID] = j
	}

	queued := make(map[string]bool)
	add := func(id string) {
		if j, ok := m.jobs[id]; ok && j.Status == JobStatusQueued && !queued[id] {
			queued[id] = true
			m.enqueueLocked(id)
		}
	}
	for _, j := range running {
		add(j.ID)
	}
	for _, id := range state.Queue {
		add(id)
	}
	// Queued jobs missing from the saved queue (shouldn't happen) go last,
	// oldest first; state.Jobs is already sorted by CreatedAt.
	for _, j := range state.Jobs {
		if j != nil {
			add(j.ID)
		}
	}
	return nil
}

// Start begins running jobs restored by loadJobs. Called once the server is
// about to accept requests, so restored downloads don't start in the middle
// of setup (or in tests that only construct a Server).
func (m *JobManager) Start() {
	m.mu.Lock()
	m.scheduleNextLocked()
	m.mu.Unlock()
}
