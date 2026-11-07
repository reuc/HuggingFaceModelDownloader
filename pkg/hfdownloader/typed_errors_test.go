// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyDownloaded_KnownSHA: a content SHA256 in the plan is always
// verified, regardless of cfg.Verify (even "none"), and a mismatch surfaces a
// typed *VerificationError.
func TestVerifyDownloaded_KnownSHA(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(f, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := computeSHA256(f)
	if err != nil {
		t.Fatal(err)
	}

	it := PlanItem{RelativePath: "a.bin", SHA256: sum, Size: 12, LFS: true}
	// Even with Verify=none, a known hash is checked and matches.
	if err := verifyDownloaded(context.Background(), nil, Settings{Verify: "none"}, it, it, f, "a.bin"); err != nil {
		t.Fatalf("known-SHA match should pass under Verify=none, got %v", err)
	}

	bad := it
	bad.SHA256 = strings.Repeat("0", 64)
	err = verifyDownloaded(context.Background(), nil, Settings{Verify: "none"}, bad, bad, f, "a.bin")
	ve, ok := errors.AsType[*VerificationError](err)
	if !ok {
		t.Fatalf("want *VerificationError, got %T (%v)", err, err)
	}
	if ve.Method != "sha256" || ve.Path != "a.bin" {
		t.Errorf("VerificationError = %+v, want Method=sha256 Path=a.bin", ve)
	}
}

// TestVerifyDownloaded_SizeAndNone: size mode catches a size mismatch; none
// mode verifies nothing.
func TestVerifyDownloaded_SizeAndNone(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(f, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}

	ok := PlanItem{RelativePath: "b.txt", Size: 5}
	if err := verifyDownloaded(context.Background(), nil, Settings{Verify: "size"}, ok, ok, f, "b.txt"); err != nil {
		t.Fatalf("size match should pass, got %v", err)
	}

	wrong := PlanItem{RelativePath: "b.txt", Size: 99}
	err := verifyDownloaded(context.Background(), nil, Settings{Verify: "size"}, wrong, wrong, f, "b.txt")
	if ve, isVE := errors.AsType[*VerificationError](err); !isVE || ve.Method != "size" {
		t.Fatalf("want size *VerificationError, got %T (%v)", err, err)
	}

	// none: even a wrong expected size is not verified.
	if err := verifyDownloaded(context.Background(), nil, Settings{Verify: "none"}, wrong, wrong, f, "b.txt"); err != nil {
		t.Errorf("Verify=none must not verify, got %v", err)
	}
}

// TestVerifyDownloaded_EtagModeUsesServerHash proves the previously-silent
// "etag" mode now actually verifies: it fetches the server's content hash via
// HEAD and catches a same-size byte flip a size check would miss.
func TestVerifyDownloaded_EtagModeUsesServerHash(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.bin")
	content := []byte("payload!") // 8 bytes
	if err := os.WriteFile(f, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := computeSHA256(f)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("x-amz-meta-sha256", sum)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// No plan SHA256 — forces the cfg.Verify path.
	it := PlanItem{RelativePath: "c.bin", URL: srv.URL + "/c.bin", Size: int64(len(content))}

	if err := verifyDownloaded(context.Background(), srv.Client(), Settings{Verify: "etag"}, it, it, f, "c.bin"); err != nil {
		t.Fatalf("etag verify should pass for an intact file, got %v", err)
	}

	// Flip one byte but keep the size identical: a size check would pass, the
	// server-hash check must fail.
	if err := os.WriteFile(f, []byte("payload?"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = verifyDownloaded(context.Background(), srv.Client(), Settings{Verify: "etag"}, it, it, f, "c.bin")
	if ve, ok := errors.AsType[*VerificationError](err); !ok || ve.Method != "etag" {
		t.Fatalf("etag verify should fail with an etag *VerificationError on a same-size corruption, got %T (%v)", err, err)
	}
}

// TestAPIError_ErrorsIs confirms HTTP status errors map onto the exported
// sentinels through errors.Is, including across an errors.Join boundary.
func TestAPIError_ErrorsIs(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{401, ErrUnauthorized},
		{403, ErrUnauthorized},
		{404, ErrNotFound},
		{429, ErrRateLimited},
	}
	for _, tc := range cases {
		err := error(&APIError{StatusCode: tc.status, Status: fmt.Sprintf("%d", tc.status)})
		if !errors.Is(err, tc.want) {
			t.Errorf("APIError{%d} errors.Is %v = false, want true", tc.status, tc.want)
		}
	}

	// Joined (as the downloader aggregates) still traverses.
	joined := errors.Join(fmt.Errorf("other"), &APIError{StatusCode: 404})
	if !errors.Is(joined, ErrNotFound) {
		t.Error("errors.Join should preserve errors.Is for APIError -> ErrNotFound")
	}
}

// TestDownloadError_Unwrap confirms a wrapped download error still exposes its
// cause via errors.Is/As.
func TestDownloadError_Unwrap(t *testing.T) {
	de := error(&DownloadError{Path: "weights.safetensors", Err: &APIError{StatusCode: 404}})
	if !errors.Is(de, ErrNotFound) {
		t.Error("DownloadError should unwrap to its APIError cause (ErrNotFound)")
	}
	if ae, ok := errors.AsType[*APIError](de); !ok || ae.StatusCode != 404 {
		t.Errorf("errors.AsType to *APIError failed: %+v", ae)
	}
}
