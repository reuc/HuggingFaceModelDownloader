// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bodaay/HuggingFaceModelDownloader/pkg/hfdownloader"
)

// The mirror endpoints resolved "target" as a name *or a raw path*, and the
// add-target endpoint stored any path. Together they let any client copy the
// cache to, read from, or (with deleteExtra) delete under any directory on
// the server. Operations now take registered names only, and web-added
// targets are confined to --mirror-dir.

func mirrorTestServer(t *testing.T, mirrorDir string) (http.Handler, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // targets file lives under HOME
	cacheDir := t.TempDir()
	// One cached repo so push/pull have something to copy.
	blobs := filepath.Join(cacheDir, "hub", "models--owner--repo", "blobs")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobs, "abc"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.CacheDir = cacheDir
	cfg.MirrorDir = mirrorDir
	srv := New(cfg)
	mux := http.NewServeMux()
	srv.registerAPIRoutes(mux)
	return mux, cacheDir
}

func registerTarget(t *testing.T, name, path string) {
	t.Helper()
	cfg, err := hfdownloader.LoadTargets("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Add(name, path, "")
	if err := cfg.Save(""); err != nil {
		t.Fatal(err)
	}
}

func postJSON(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func TestMirror_RejectsRawPathTargets(t *testing.T) {
	mux, _ := mirrorTestServer(t, "")
	outside := t.TempDir()
	for _, ep := range []string{"/api/mirror/diff", "/api/mirror/push", "/api/mirror/pull"} {
		w := postJSON(mux, "POST", ep, `{"target":`+jsonString(outside)+`,"deleteExtra":true}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s with raw path: status %d body %s; want 400", ep, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "registered target") {
			t.Errorf("%s: body should explain targets must be registered: %s", ep, w.Body)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("raw-path push wrote into %s: %v", outside, entries)
	}
	// An unknown name is rejected the same way.
	w := postJSON(mux, "POST", "/api/mirror/push", `{"target":"nope"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown target name: status %d; want 400", w.Code)
	}
}

func TestMirror_PushUsesRegisteredTarget(t *testing.T) {
	mux, _ := mirrorTestServer(t, "")
	dest := t.TempDir()
	registerTarget(t, "nas", dest)

	w := postJSON(mux, "POST", "/api/mirror/push", `{"target":"nas"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("push to registered target: status %d body %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(dest, "hub", "models--owner--repo", "blobs", "abc")); err != nil {
		t.Errorf("blob not copied to registered target: %v", err)
	}
}

func TestMirror_TargetAdd_DisabledWithoutMirrorDir(t *testing.T) {
	mux, _ := mirrorTestServer(t, "")
	w := postJSON(mux, "POST", "/api/mirror/targets", `{"name":"x","path":"/"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "--mirror-dir") {
		t.Errorf("status %d body %s; want 400 explaining --mirror-dir", w.Code, w.Body)
	}
	cfg, _ := hfdownloader.LoadTargets("")
	if _, ok := cfg.Get("x"); ok {
		t.Error("target was stored although adding is disabled")
	}
}

func TestMirror_TargetAdd_ConfinedToMirrorDir(t *testing.T) {
	mirrorDir := t.TempDir()
	mux, _ := mirrorTestServer(t, mirrorDir)

	bad := []string{
		"/",
		t.TempDir(),
		filepath.Join(mirrorDir, "..", "escape"),
		mirrorDir + "-sibling",
	}
	for _, p := range bad {
		w := postJSON(mux, "POST", "/api/mirror/targets", `{"name":"bad","path":`+jsonString(p)+`}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("path %q: status %d body %s; want 400", p, w.Code, w.Body)
		}
	}
	cfg, _ := hfdownloader.LoadTargets("")
	if _, ok := cfg.Get("bad"); ok {
		t.Error("a path outside --mirror-dir was stored")
	}

	inside := filepath.Join(mirrorDir, "usb")
	w := postJSON(mux, "POST", "/api/mirror/targets", `{"name":"usb","path":`+jsonString(inside)+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("path inside --mirror-dir: status %d body %s", w.Code, w.Body)
	}
	cfg, _ = hfdownloader.LoadTargets("")
	if tgt, ok := cfg.Get("usb"); !ok || tgt.Path != inside {
		t.Errorf("target not stored as %s: %+v", inside, cfg.Targets)
	}

	// A symlink inside --mirror-dir pointing outside must not let a target
	// escape (prefixes are resolved before comparing).
	outside := t.TempDir()
	link := filepath.Join(mirrorDir, "link")
	if err := os.Symlink(outside, link); err == nil {
		w := postJSON(mux, "POST", "/api/mirror/targets", `{"name":"viaLink","path":`+jsonString(filepath.Join(link, "x"))+`}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("symlink escape: status %d body %s; want 400", w.Code, w.Body)
		}
	}
}

func TestMirror_ServerManagedTargetsAreReadOnlyFromWeb(t *testing.T) {
	mirrorDir := t.TempDir()
	mux, _ := mirrorTestServer(t, mirrorDir)
	serverPath := t.TempDir() // outside --mirror-dir: registered by the operator
	registerTarget(t, "office", serverPath)

	// Can't be repointed (even to a path inside --mirror-dir)...
	w := postJSON(mux, "POST", "/api/mirror/targets", `{"name":"office","path":`+jsonString(filepath.Join(mirrorDir, "x"))+`}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("repoint server target: status %d body %s; want 403", w.Code, w.Body)
	}
	// ...nor removed from the web.
	w = postJSON(mux, "DELETE", "/api/mirror/targets/office", "")
	if w.Code != http.StatusForbidden {
		t.Errorf("remove server target: status %d body %s; want 403", w.Code, w.Body)
	}
	cfg, _ := hfdownloader.LoadTargets("")
	if tgt, ok := cfg.Get("office"); !ok || tgt.Path != serverPath {
		t.Errorf("server target changed: %+v", cfg.Targets)
	}
	// But it can still be used for operations.
	w = postJSON(mux, "POST", "/api/mirror/diff", `{"target":"office"}`)
	if w.Code != http.StatusOK {
		t.Errorf("diff against server target: status %d body %s", w.Code, w.Body)
	}

	// A web-added target (inside --mirror-dir) can be removed from the web.
	registerTarget(t, "usb", filepath.Join(mirrorDir, "usb"))
	w = postJSON(mux, "DELETE", "/api/mirror/targets/usb", "")
	if w.Code != http.StatusOK {
		t.Errorf("remove web target: status %d body %s", w.Code, w.Body)
	}
}

func TestMirror_SettingsReportMirrorDir(t *testing.T) {
	mirrorDir := t.TempDir()
	mux, _ := mirrorTestServer(t, mirrorDir)
	w := postJSON(mux, "GET", "/api/settings", "")
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["mirrorDir"] != mirrorDir {
		t.Errorf("mirrorDir = %v, want %s", got["mirrorDir"], mirrorDir)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
