// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every response (UI, API, errors) carries the browser hardening headers.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	srv := newTestServer()
	h := srv.handler()
	for _, path := range []string{"/", "/js/app.js", "/api/health", "/api/jobs/nope"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		got := w.Header()
		if got.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", path, got.Get("X-Content-Type-Options"))
		}
		if got.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q", path, got.Get("X-Frame-Options"))
		}
		csp := got.Get("Content-Security-Policy")
		for _, want := range []string{"object-src 'none'", "frame-ancestors 'none'", "base-uri 'self'", "connect-src 'self' ws: wss:"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", path, csp, want)
			}
		}
	}
	// The UI must still be served with its content type.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("GET /: status %d content-type %q", w.Code, w.Header().Get("Content-Type"))
	}
}
