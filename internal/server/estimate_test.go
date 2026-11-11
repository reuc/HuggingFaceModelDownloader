// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"
	"time"
)

// A queued job gets its download size from the Hub scan before it runs, so
// the UI can total the space the queue needs.
func TestQueuedJobGetsEstimatedBytes(t *testing.T) {
	m, _, release, ids := queueFixture(t, Config{}, "owner/queued")
	defer close(release)
	m.sizeFn = func(ctx context.Context, job Job, cfg Config) (int64, error) {
		if job.Repo != "owner/queued" {
			t.Errorf("scanned %q", job.Repo)
		}
		return 1234, nil
	}
	m.estimateAsync(ids["owner/queued"])

	deadline := time.Now().Add(2 * time.Second)
	for {
		j, _ := m.GetJob(ids["owner/queued"])
		if j.EstimatedBytes == 1234 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("EstimatedBytes = %d, want 1234", j.EstimatedBytes)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Without a size function (the default), no scan runs and the job is left
// unsized.
func TestEstimateDisabledByDefault(t *testing.T) {
	m, _, release, ids := queueFixture(t, Config{}, "owner/queued")
	defer close(release)
	m.estimateAsync(ids["owner/queued"])
	time.Sleep(50 * time.Millisecond)
	if j, _ := m.GetJob(ids["owner/queued"]); j.EstimatedBytes != 0 {
		t.Fatalf("EstimatedBytes = %d, want 0", j.EstimatedBytes)
	}
}
