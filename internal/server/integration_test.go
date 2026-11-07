// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// getFreePort finds an available port
func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// startIntegrationServer runs a server built from cfg (on a free local port)
// until the test ends, then stops it and fails the test if it didn't exit
// cleanly. It returns the server's base URL.
func startIntegrationServer(t *testing.T, cfg Config) string {
	t.Helper()
	cfg.Addr = "127.0.0.1"
	cfg.Port = getFreePort(t)
	srv := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("server: %v", err)
		}
	})

	time.Sleep(200 * time.Millisecond)
	return "http://127.0.0.1:" + strconv.Itoa(cfg.Port)
}

// These tests require network access and actually download from HuggingFace.
// Run with: go test -tags=integration -v ./internal/server/

func TestIntegration_FullDownloadFlow(t *testing.T) {
	baseURL := startIntegrationServer(t, Config{
		ModelsDir:   t.TempDir(),
		DatasetsDir: t.TempDir(),
		Concurrency: 4,
		MaxActive:   2,
	})

	t.Run("health check", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/health")
		if err != nil {
			t.Fatalf("Health check failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != 200 {
			t.Errorf("Expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("start download and track progress", func(t *testing.T) {
		// Start a tiny model download
		body := `{"repo": "hf-internal-testing/tiny-random-gpt2"}`
		resp, err := http.Post(
			baseURL+"/api/download",
			"application/json",
			bytes.NewBufferString(body),
		)
		if err != nil {
			t.Fatalf("Start download failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != 202 {
			t.Fatalf("Expected 202, got %d", resp.StatusCode)
		}

		var job Job
		if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
			t.Fatalf("decode job: %v", err)
		}

		if job.ID == "" {
			t.Error("Job ID should not be empty")
		}

		// Poll for completion
		timeout := time.After(60 * time.Second)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-timeout:
				t.Fatal("Download timed out")
			case <-ticker.C:
				jobResp, err := http.Get(baseURL + "/api/jobs/" + job.ID)
				if err != nil {
					t.Fatalf("get job: %v", err)
				}
				var current Job
				err = json.NewDecoder(jobResp.Body).Decode(&current)
				_ = jobResp.Body.Close()
				if err != nil {
					t.Fatalf("decode job: %v", err)
				}

				t.Logf("Job status: %s, progress: %d/%d files",
					current.Status, current.Progress.CompletedFiles, current.Progress.TotalFiles)

				if current.Status == JobStatusCompleted {
					t.Log("Download completed successfully!")
					return
				}
				if current.Status == JobStatusFailed {
					t.Fatalf("Download failed: %s", current.Error)
				}
			}
		}
	})
}

func TestIntegration_DryRun(t *testing.T) {
	baseURL := startIntegrationServer(t, Config{
		ModelsDir:   t.TempDir(),
		DatasetsDir: t.TempDir(),
	})

	body := `{"repo": "hf-internal-testing/tiny-random-gpt2"}`
	resp, err := http.Post(
		baseURL+"/api/plan",
		"application/json",
		bytes.NewBufferString(body),
	)
	if err != nil {
		t.Fatalf("Plan request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Fatalf("Expected 200, got %d", resp.StatusCode)
	}

	var plan PlanResponse
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}

	if plan.TotalFiles == 0 {
		t.Error("Expected files in plan")
	}
	t.Logf("Plan: %d files, %d bytes", plan.TotalFiles, plan.TotalSize)

	for _, f := range plan.Files {
		t.Logf("  %s (%d bytes, LFS=%v)", f.Path, f.Size, f.LFS)
	}
}
