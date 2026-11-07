// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reuc/HuggingFaceModelDownloader/pkg/hfdownloader"
)

const resumeTestCommit = "0123456789abcdef0123456789abcdef01234567"

// resumeHub is a minimal Hub for repo o/r whose file responses can be made to
// fail partway: cut drops the connection after n body bytes (a network
// failure), hang sends n bytes and then stalls until the client gives up.
type resumeHub struct {
	lfs   map[string][]byte // served from /resolve/
	small map[string][]byte // served from /raw/

	mu      sync.Mutex
	cut     map[string]int
	hang    map[string]int
	started chan struct{} // receives once per hanging response, after its bytes
	ranges  []string      // "<path> <Range header>" of each file GET
}

func newResumeHub(lfs, small map[string][]byte) *resumeHub {
	return &resumeHub{lfs: lfs, small: small, cut: map[string]int{}, hang: map[string]int{}, started: make(chan struct{}, 16)}
}

func (h *resumeHub) setCut(path string, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n <= 0 {
		delete(h.cut, path)
	} else {
		h.cut[path] = n
	}
}

func (h *resumeHub) setHang(path string, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n <= 0 {
		delete(h.hang, path)
	} else {
		h.hang[path] = n
	}
}

// takeRanges returns and clears the recorded GET ranges.
func (h *resumeHub) takeRanges() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.ranges
	h.ranges = nil
	return r
}

func (h *resumeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/models/o/r/revision/"):
		json.NewEncoder(w).Encode(map[string]string{"sha": resumeTestCommit})
	case strings.HasPrefix(p, "/api/models/o/r/tree/"):
		var nodes []map[string]any
		for path, data := range h.lfs {
			sum := sha256.Sum256(data)
			nodes = append(nodes, map[string]any{"type": "file", "path": path, "size": 134,
				"lfs": map[string]any{"oid": hex.EncodeToString(sum[:]), "size": len(data)}})
		}
		for path, data := range h.small {
			oid := sha1.New()
			fmt.Fprintf(oid, "blob %d\x00", len(data))
			oid.Write(data)
			nodes = append(nodes, map[string]any{"type": "file", "path": path, "size": len(data), "oid": hex.EncodeToString(oid.Sum(nil))})
		}
		json.NewEncoder(w).Encode(nodes)
	case strings.HasPrefix(p, "/o/r/resolve/"), strings.HasPrefix(p, "/o/r/raw/"):
		route := "/o/r/resolve/"
		files := h.lfs
		if strings.HasPrefix(p, "/o/r/raw/") {
			route, files = "/o/r/raw/", h.small
		}
		_, path, _ := strings.Cut(strings.TrimPrefix(p, route), "/")
		data, ok := files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var out http.ResponseWriter = w
		if r.Method == http.MethodGet {
			h.mu.Lock()
			h.ranges = append(h.ranges, path+" "+r.Header.Get("Range"))
			cut, hang := h.cut[path], h.hang[path]
			h.mu.Unlock()
			switch {
			case cut > 0:
				out = &failingWriter{ResponseWriter: w, left: cut}
			case hang > 0:
				out = &failingWriter{ResponseWriter: w, left: hang, hang: r.Context(), started: h.started}
			}
		}
		http.ServeContent(out, r, path, time.Time{}, bytes.NewReader(data))
	default:
		http.NotFound(w, r)
	}
}

// failingWriter passes through `left` body bytes, then either drops the
// connection or (with hang set) stalls until the request is cancelled.
type failingWriter struct {
	http.ResponseWriter
	left    int
	hang    context.Context
	started chan struct{}
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if len(p) <= f.left {
		f.left -= len(p)
		return f.ResponseWriter.Write(p)
	}
	f.ResponseWriter.Write(p[:f.left])
	f.left = 0
	if fl, ok := f.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
	if f.hang != nil {
		f.started <- struct{}{}
		<-f.hang.Done()
		return 0, f.hang.Err()
	}
	panic(http.ErrAbortHandler)
}

func testPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/251)
	}
	return b
}

// runCLI runs hfdownloader with args, like Execute but without exiting.
func runCLI(ctx context.Context, args ...string) (string, error) {
	root := newRootCmd(ctx, "test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

// isolateHome gives the test its own HOME, so records and config files from
// other tests (or the developer's machine) are never seen.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func blobFile(cache string, data []byte) string {
	sum := sha256.Sum256(data)
	return filepath.Join(cache, "hub", "models--o--r", "blobs", hex.EncodeToString(sum[:]))
}

func partFile(cache string, data []byte) string {
	sum := sha256.Sum256(data)
	return filepath.Join(cache, "hub", "models--o--r", "blobs", "tmp-"+hex.EncodeToString(sum[:])+".part")
}

func mustResumable(t *testing.T, err error) string {
	t.Helper()
	var re *resumableError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want a resumable download error", err)
	}
	return re.id
}

// A download that fails partway is recorded; `resume` (no arguments) picks
// it up with the original settings, continues the partial file with a Range
// request instead of starting over, and clears the record once complete.
func TestResume_ContinuesFailedDownload(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	weights := testPayload(200000)
	config := []byte(`{"model_type": "test"}`)
	hub := newResumeHub(map[string][]byte{"model.safetensors": weights}, map[string][]byte{"config.json": config})
	hub.setCut("model.safetensors", 70000)
	srv := httptest.NewServer(hub)
	defer srv.Close()
	cache := t.TempDir()
	store := defaultRecordStore()

	// 1. The first attempt loses its connection partway through the weights.
	_, err := runCLI(ctx, "download", "o/r", "--cache-dir", cache, "--endpoint", srv.URL,
		"-q", "--retries", "0", "-t", "hf_secret_test_token")
	id := mustResumable(t, err)

	rec := store.load(id)
	if rec == nil {
		t.Fatalf("no record %s after a failed download", id)
	}
	if rec.Status != statusFailed || rec.Attempts != 1 || !strings.Contains(rec.Error, "model.safetensors") {
		t.Errorf("record = status %q, attempts %d, error %q", rec.Status, rec.Attempts, rec.Error)
	}
	if rec.Progress.FilesTotal != 2 || rec.Progress.FilesDone != 1 || rec.Progress.BytesTotal != int64(len(weights)+len(config)) {
		t.Errorf("progress = %+v, want 1 of 2 files, %d bytes total", rec.Progress, len(weights)+len(config))
	}
	if rec.Settings.CacheDir != cache || rec.Settings.Endpoint != srv.URL || rec.Settings.Retries != 0 {
		t.Errorf("settings = %+v", rec.Settings)
	}
	raw, _ := os.ReadFile(filepath.Join(store.dir, id+".json"))
	if bytes.Contains(raw, []byte("hf_secret_test_token")) {
		t.Fatal("the access token was written to the download record")
	}
	part1 := fileSize(t, partFile(cache, weights))
	if part1 <= 0 || part1 >= int64(len(weights)) {
		t.Fatalf("partial file has %d bytes, want some but not all", part1)
	}
	hub.takeRanges()

	// 2. A resume with other tuning fails again, but continues the partial
	// file, counts the attempt and records the new settings.
	_, err = runCLI(ctx, "resume", "-q", "-c", "3", "--retries", "0")
	if got := mustResumable(t, err); got != id {
		t.Fatalf("resume reported id %s, want %s", got, id)
	}
	if want := fmt.Sprintf("model.safetensors bytes=%d-%d", part1, len(weights)-1); !contains(hub.takeRanges(), want) {
		t.Errorf("second attempt did not continue from the partial file (want GET %q)", want)
	}
	rec = store.load(id)
	if rec == nil || rec.Attempts != 2 || rec.Settings.Concurrency != 3 || rec.Status != statusFailed {
		t.Fatalf("record after second attempt = %+v", rec)
	}
	part2 := fileSize(t, partFile(cache, weights))
	if part2 <= part1 {
		t.Fatalf("partial file did not grow: %d -> %d bytes", part1, part2)
	}

	// 3. With the network fixed, resume finishes the file from where it stopped.
	hub.setCut("model.safetensors", 0)
	out, err := runCLI(ctx, "resume", "-q")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !strings.Contains(out, "Resuming "+id) || !strings.Contains(out, "(attempt 3)") {
		t.Errorf("resume output = %q", out)
	}
	ranges := hub.takeRanges()
	if want := fmt.Sprintf("model.safetensors bytes=%d-%d", part2, len(weights)-1); !contains(ranges, want) {
		t.Errorf("final attempt GETs = %v, want %q", ranges, want)
	}
	for _, r := range ranges {
		if strings.HasPrefix(r, "config.json") {
			t.Errorf("resume downloaded the completed config.json again")
		}
	}
	got, err := os.ReadFile(blobFile(cache, weights))
	if err != nil || !bytes.Equal(got, weights) {
		t.Fatalf("resumed file is wrong (err %v, %d bytes)", err, len(got))
	}
	if store.load(id) != nil {
		t.Error("the record of a completed download was not removed")
	}
	if out, _ := runCLI(ctx, "resume"); !strings.Contains(out, "No failed or interrupted downloads") {
		t.Errorf("resume with nothing left = %q", out)
	}
}

// Multipart downloads keep each part's bytes; a resume requests only the rest
// of every part.
func TestResume_ContinuesMultipartDownload(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	weights := testPayload(200000)
	hub := newResumeHub(map[string][]byte{"model.safetensors": weights}, nil)
	hub.setCut("model.safetensors", 30000)
	srv := httptest.NewServer(hub)
	defer srv.Close()
	cache := t.TempDir()

	_, err := runCLI(ctx, "download", "o/r", "--cache-dir", cache, "--endpoint", srv.URL,
		"-q", "--retries", "0", "-c", "2", "--multipart-threshold", "1KiB")
	mustResumable(t, err)
	hub.takeRanges()

	hub.setCut("model.safetensors", 0)
	if _, err := runCLI(ctx, "resume", "-q"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	for _, r := range hub.takeRanges() {
		if r == "model.safetensors bytes=0-99999" || r == "model.safetensors bytes=100000-199999" {
			t.Errorf("resume restarted a part from the beginning: GET %q", r)
		}
	}
	if got, _ := os.ReadFile(blobFile(cache, weights)); !bytes.Equal(got, weights) {
		t.Fatal("resumed multipart file is wrong")
	}
}

// Ctrl+C (context cancellation) records the download as interrupted.
func TestResume_InterruptedDownload(t *testing.T) {
	isolateHome(t)
	weights := testPayload(200000)
	hub := newResumeHub(map[string][]byte{"model.safetensors": weights}, nil)
	hub.setHang("model.safetensors", 50000)
	srv := httptest.NewServer(hub)
	defer srv.Close()
	cache := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-hub.started
		time.Sleep(50 * time.Millisecond) // let the client take the bytes
		cancel()
	}()
	_, err := runCLI(ctx, "download", "o/r", "--cache-dir", cache, "--endpoint", srv.URL, "-q", "--stall-timeout", "0")
	id := mustResumable(t, err)
	if rec := defaultRecordStore().load(id); rec == nil || rec.Status != statusInterrupted {
		t.Fatalf("record = %+v, want status interrupted", rec)
	}

	hub.setHang("model.safetensors", 0)
	if _, err := runCLI(context.Background(), "resume", "-q", id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, _ := os.ReadFile(blobFile(cache, weights)); !bytes.Equal(got, weights) {
		t.Fatal("resumed file is wrong")
	}
}

// The record holds absolute paths: a flat --local-dir download resumed from
// another working directory still writes where the original command would.
func TestResume_RelativeLocalDirFromAnotherDirectory(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	weights := testPayload(100000)
	hub := newResumeHub(map[string][]byte{"model.safetensors": weights}, nil)
	hub.setCut("model.safetensors", 40000)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	first := t.TempDir()
	t.Chdir(first)
	_, err := runCLI(ctx, "download", "o/r", "--local-dir", "out", "--endpoint", srv.URL, "-q", "--retries", "0")
	id := mustResumable(t, err)

	other := t.TempDir()
	t.Chdir(other)
	hub.setCut("model.safetensors", 0)
	if _, err := runCLI(ctx, "resume", "-q", "o/r"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(first, "out", "o", "r", "model.safetensors")); !bytes.Equal(got, weights) {
		t.Fatal("resume did not finish the file in the original --local-dir")
	}
	if _, err := os.Stat(filepath.Join(other, "out")); !os.IsNotExist(err) {
		t.Error("resume wrote into the current directory instead of the recorded one")
	}
	if defaultRecordStore().load(id) != nil {
		t.Error("record not removed after the resume completed")
	}
}

// A download that fails before anything could be fetched (here: the repo
// doesn't exist) leaves nothing to resume.
func TestResume_NothingRecordedWhenScanFails(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(newResumeHub(nil, nil))
	defer srv.Close()

	_, err := runCLI(context.Background(), "download", "o/missing", "--cache-dir", t.TempDir(), "--endpoint", srv.URL, "-q", "--retries", "0")
	if err == nil {
		t.Fatal("download of a missing repo succeeded")
	}
	var re *resumableError
	if errors.As(err, &re) {
		t.Errorf("a failed scan was reported as resumable (%s)", re.id)
	}
	if recs, _ := defaultRecordStore().list(); len(recs) != 0 {
		t.Errorf("records = %d, want none", len(recs))
	}
}

func TestResume_ListAndForget(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	weights := testPayload(100000)
	hub := newResumeHub(map[string][]byte{"model.safetensors": weights}, nil)
	hub.setCut("model.safetensors", 40000)
	srv := httptest.NewServer(hub)
	defer srv.Close()
	cache := t.TempDir()

	_, err := runCLI(ctx, "download", "o/r", "--cache-dir", cache, "--endpoint", srv.URL, "-q", "--retries", "0", "-F", "safetensors")
	id := mustResumable(t, err)

	out, err := runCLI(ctx, "resume", "--list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{id, "failed", "o/r [safetensors]", "0/1 files", "error:"} {
		if !strings.Contains(out, want) {
			t.Errorf("--list output lacks %q:\n%s", want, out)
		}
	}

	out, err = runCLI(ctx, "resume", "--list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var listed []struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Job   struct {
			Repo string `json:"repo"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil || len(listed) != 1 || listed[0].ID != id || listed[0].State != statusFailed || listed[0].Job.Repo != "o/r" {
		t.Fatalf("--list --json = %s (err %v)", out, err)
	}

	if _, err := runCLI(ctx, "resume", "--forget"); err == nil {
		t.Error("--forget without an ID or --all was accepted")
	}
	if _, err := runCLI(ctx, "resume", "--forget", id[:6]); err != nil {
		t.Fatalf("--forget: %v", err)
	}
	if defaultRecordStore().load(id) != nil {
		t.Error("--forget left the record")
	}
	part := fileSize(t, partFile(cache, weights)) // --forget keeps the partial data
	// A later download of the same files still continues the partial file.
	hub.setCut("model.safetensors", 0)
	hub.takeRanges()
	if _, err := runCLI(ctx, "download", "o/r", "--cache-dir", cache, "--endpoint", srv.URL, "-q", "-F", "safetensors"); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("model.safetensors bytes=%d-%d", part, len(weights)-1); !contains(hub.takeRanges(), want) {
		t.Errorf("re-run did not continue the partial file (want GET %q)", want)
	}
}

// While a download runs, its record says so and is kept fresh, and a resume
// of the same download is refused (two writers would corrupt the partial
// files); once interrupted, it is resumable.
func TestResume_LiveDownloadHeartbeatsAndBlocksResume(t *testing.T) {
	isolateHome(t)
	saved := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	defer func() { heartbeatInterval = saved }()

	weights := testPayload(200000)
	hub := newResumeHub(map[string][]byte{"model.safetensors": weights}, nil)
	hub.setHang("model.safetensors", 50000)
	srv := httptest.NewServer(hub)
	defer srv.Close()
	store := defaultRecordStore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := runCLI(ctx, "download", "o/r", "--cache-dir", t.TempDir(), "--endpoint", srv.URL, "-q", "--stall-timeout", "0")
		errc <- err
	}()
	<-hub.started

	recs, _ := store.list()
	if len(recs) != 1 || recs[0].state(time.Now()) != statusRunning || recs[0].PID != os.Getpid() {
		t.Fatalf("records while downloading = %+v", recs)
	}
	id, first := recs[0].ID, recs[0].UpdatedAt
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r := store.load(id); r != nil && r.UpdatedAt.After(first) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the running download's record was not refreshed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := runCLI(context.Background(), "resume", id); err == nil || !strings.Contains(err.Error(), "still downloading") {
		t.Fatalf("resume during the download: err = %v", err)
	}

	cancel()
	mustResumable(t, <-errc)
	if r := store.load(id); r == nil || r.state(time.Now()) != statusInterrupted || r.PID != 0 {
		t.Fatalf("record after the interruption = %+v", r)
	}
}

// A download still running in another process can't be resumed alongside it.
func TestResume_RefusesDownloadStillRunning(t *testing.T) {
	isolateHome(t)
	store := defaultRecordStore()
	rec := newDownloadRecord(hfdownloader.Job{Repo: "o/r"}, hfdownloader.Settings{CacheDir: t.TempDir()})
	rec.Attempts = 1 // newDownloadRecord: this (live) process, status running, updated now
	if err := store.save(rec); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(context.Background(), "resume", rec.ID)
	if err == nil || !strings.Contains(err.Error(), "still downloading") {
		t.Fatalf("resume of a running download: err = %v", err)
	}
	if out, err := runCLI(context.Background(), "resume"); err != nil || !strings.Contains(out, "No failed or interrupted downloads") {
		t.Fatalf("resume with only a running download = %q, %v", out, err)
	}
	if store.load(rec.ID) == nil {
		t.Error("the running download's record was removed")
	}
}

func TestDownloadRecord_State(t *testing.T) {
	host, _ := os.Hostname()
	now := time.Now()
	for _, tc := range []struct {
		name string
		rec  downloadRecord
		want string
	}{
		{"failed", downloadRecord{Status: statusFailed}, statusFailed},
		{"interrupted", downloadRecord{Status: statusInterrupted}, statusInterrupted},
		{"live process", downloadRecord{Status: statusRunning, PID: os.Getpid(), Host: host, UpdatedAt: now}, statusRunning},
		{"process gone", downloadRecord{Status: statusRunning, PID: 0, Host: host, UpdatedAt: now}, statusInterrupted},
		{"no heartbeat", downloadRecord{Status: statusRunning, PID: os.Getpid(), Host: host, UpdatedAt: now.Add(-staleAfter - time.Second)}, statusInterrupted},
		{"other machine, recent", downloadRecord{Status: statusRunning, PID: 1, Host: host + "-elsewhere", UpdatedAt: now}, statusRunning},
		{"other machine, stale", downloadRecord{Status: statusRunning, PID: 1, Host: host + "-elsewhere", UpdatedAt: now.Add(-time.Hour)}, statusInterrupted},
	} {
		if got := tc.rec.state(now); got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPickRecords(t *testing.T) {
	now := time.Now()
	failed := func(id, repo string) *downloadRecord {
		return &downloadRecord{ID: id, Status: statusFailed, Job: recordJob{Repo: repo, Revision: "main"}}
	}
	a := failed("aaaa11112222", "owner/one")
	b := failed("bbbb11112222", "owner/two")
	b2 := failed("bbbb33334444", "owner/two")
	b2.Job.Revision = "dev"
	host, _ := os.Hostname()
	live := &downloadRecord{ID: "cccc11112222", Status: statusRunning, PID: os.Getpid(), Host: host, UpdatedAt: now, Job: recordJob{Repo: "owner/three"}}

	ids := func(rs []*downloadRecord) string {
		var s []string
		for _, r := range rs {
			s = append(s, r.ID)
		}
		return strings.Join(s, ",")
	}
	for _, tc := range []struct {
		name    string
		recs    []*downloadRecord
		arg     string
		all     bool
		want    string
		wantErr string
	}{
		{"nothing recorded", nil, "", false, "", ""},
		{"the only one", []*downloadRecord{a}, "", false, a.ID, ""},
		{"only running ones", []*downloadRecord{live}, "", false, "", ""},
		{"one besides a running one", []*downloadRecord{live, a}, "", false, a.ID, ""},
		{"several", []*downloadRecord{a, b}, "", false, "", "2 downloads can be resumed"},
		{"all", []*downloadRecord{a, live, b}, "", true, a.ID + "," + b.ID, ""},
		{"full id", []*downloadRecord{a, b}, b.ID, false, b.ID, ""},
		{"id prefix", []*downloadRecord{a, b}, "AAAA", false, a.ID, ""},
		{"prefix too short", []*downloadRecord{a, b}, "aaa", false, "", "no failed or interrupted download matches"},
		{"repo", []*downloadRecord{a, b}, "Owner/One", false, a.ID, ""},
		{"repo with several revisions", []*downloadRecord{a, b, b2}, "owner/two", false, "", `"owner/two" matches 2 downloads`},
		{"repo@revision", []*downloadRecord{a, b, b2}, "owner/two@dev", false, b2.ID, ""},
		{"all of a repo", []*downloadRecord{a, b, b2}, "owner/two", true, b.ID + "," + b2.ID, ""},
		{"no match", []*downloadRecord{a}, "owner/zzz", false, "", "no failed or interrupted download matches"},
		{"running by id", []*downloadRecord{a, live}, live.ID, false, "", "still downloading"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickRecords(tc.recs, tc.arg, tc.all, now)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ids(got) != tc.want {
				t.Errorf("picked %q, want %q", ids(got), tc.want)
			}
		})
	}
}

func TestRecordID(t *testing.T) {
	base := func() (hfdownloader.Job, hfdownloader.Settings) {
		return hfdownloader.Job{Repo: "o/r", Filters: []string{"q4_k_m", "q5_k_m"}},
			hfdownloader.Settings{CacheDir: "/cache", Concurrency: 8}
	}
	job, cfg := base()
	id := newDownloadRecord(job, cfg).ID
	if !recordIDPattern.MatchString(id) {
		t.Fatalf("id %q", id)
	}

	same := map[string]func(*hfdownloader.Job, *hfdownloader.Settings){
		"filter order":   func(j *hfdownloader.Job, _ *hfdownloader.Settings) { j.Filters = []string{"q5_k_m", "q4_k_m"} },
		"main revision":  func(j *hfdownloader.Job, _ *hfdownloader.Settings) { j.Revision = "main" },
		"connections":    func(_ *hfdownloader.Job, s *hfdownloader.Settings) { s.Concurrency = 2 },
		"retries":        func(_ *hfdownloader.Job, s *hfdownloader.Settings) { s.Retries = 20 },
		"flatten":        func(_ *hfdownloader.Job, s *hfdownloader.Settings) { s.Flatten = true },
		"token":          func(_ *hfdownloader.Job, s *hfdownloader.Settings) { s.Token = "hf_x" },
		"relative cache": func(_ *hfdownloader.Job, s *hfdownloader.Settings) { s.CacheDir = "/cache/../cache" },
	}
	differs := map[string]func(*hfdownloader.Job, *hfdownloader.Settings){
		"repo":      func(j *hfdownloader.Job, _ *hfdownloader.Settings) { j.Repo = "o/other" },
		"revision":  func(j *hfdownloader.Job, _ *hfdownloader.Settings) { j.Revision = "dev" },
		"filters":   func(j *hfdownloader.Job, _ *hfdownloader.Settings) { j.Filters = []string{"q4_k_m"} },
		"excludes":  func(j *hfdownloader.Job, _ *hfdownloader.Settings) { j.Excludes = []string{".md"} },
		"dataset":   func(j *hfdownloader.Job, _ *hfdownloader.Settings) { j.IsDataset = true },
		"shards":    func(j *hfdownloader.Job, _ *hfdownloader.Settings) { j.Shards = "1-3" },
		"cache dir": func(_ *hfdownloader.Job, s *hfdownloader.Settings) { s.CacheDir = "/other" },
		"local dir": func(_ *hfdownloader.Job, s *hfdownloader.Settings) { s.CacheDir, s.OutputDir = "", "/cache" },
	}
	for name, change := range same {
		j, s := base()
		change(&j, &s)
		if got := newDownloadRecord(j, s).ID; got != id {
			t.Errorf("%s changed the id (%s != %s)", name, got, id)
		}
	}
	for name, change := range differs {
		j, s := base()
		change(&j, &s)
		if got := newDownloadRecord(j, s).ID; got == id {
			t.Errorf("%s did not change the id", name)
		}
	}
}

// Credentials never reach the record, and the record round-trips the rest.
func TestDownloadRecord_NoSecretsAndRoundTrip(t *testing.T) {
	job := hfdownloader.Job{Repo: "o/r", IsDataset: true, Revision: "v1", Filters: []string{"a"}, Excludes: []string{"b"},
		Shards: "1-2", ExactMatch: true, AppendFilterSubdir: true}
	cfg := hfdownloader.Settings{CacheDir: "/c", Concurrency: 3, MaxActiveDownloads: 2, MultipartThreshold: "1MiB",
		Verify: "sha256", Retries: 7, BackoffInitial: "1s", BackoffMax: "5s", StallTimeout: "30s", LinkMode: "copy",
		Endpoint: "https://hf-mirror.example", NoManifest: true, Flatten: true, StaleTimeout: "9m",
		Token: "hf_secret", Command: "hfdownloader download o/r",
		Proxy: &hfdownloader.ProxyConfig{URL: "http://alice:pa55@proxy.example:3128", Username: "alice", Password: "pa55", NoEnvProxy: true}}
	rec := newDownloadRecord(job, cfg)
	raw, _ := json.Marshal(rec)
	for _, secret := range []string{"hf_secret", "pa55", "alice"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("record contains %q: %s", secret, raw)
		}
	}

	if got := rec.job(); fmt.Sprint(got) != fmt.Sprint(job) {
		t.Errorf("job round trip = %+v, want %+v", got, job)
	}
	got := rec.settings()
	want := cfg
	want.Token = ""
	want.Proxy = &hfdownloader.ProxyConfig{URL: "http://proxy.example:3128", NoEnvProxy: true}
	if fmt.Sprint(*got.Proxy) != fmt.Sprint(*want.Proxy) {
		t.Errorf("proxy round trip = %+v, want %+v", *got.Proxy, *want.Proxy)
	}
	got.Proxy, want.Proxy = nil, nil
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("settings round trip =\n %+v\nwant\n %+v", got, want)
	}
}

func TestResumeProxy(t *testing.T) {
	cmd := newResumeCmd(context.Background(), &RootOpts{})
	o := &resumeOpts{}
	fromConfig := &hfdownloader.ProxyConfig{URL: "http://config:1", Username: "u", Password: "p", NoProxy: "localhost"}

	// Recorded URL wins over the config file; config credentials are kept.
	got := resumeProxy(cmd, o, fromConfig, &hfdownloader.ProxyConfig{URL: "http://recorded:2"})
	if got.URL != "http://recorded:2" || got.Username != "u" || got.Password != "p" || got.NoProxy != "localhost" {
		t.Errorf("recorded+config = %+v", *got)
	}
	if fromConfig.URL != "http://config:1" {
		t.Error("resumeProxy modified the config's proxy settings")
	}
	// Nothing recorded: the config file as is.
	if got := resumeProxy(cmd, o, fromConfig, nil); got.URL != "http://config:1" {
		t.Errorf("config only = %+v", *got)
	}
	// Flags win over both.
	cmd.ParseFlags([]string{"--proxy", "http://flag:3", "--proxy-pass", "flagpass"})
	o.proxyURL, o.proxyPass = "http://flag:3", "flagpass"
	if got := resumeProxy(cmd, o, fromConfig, &hfdownloader.ProxyConfig{URL: "http://recorded:2"}); got.URL != "http://flag:3" || got.Password != "flagpass" || got.Username != "u" {
		t.Errorf("flags = %+v", *got)
	}
}

func TestRecordStore_IgnoresBadFiles(t *testing.T) {
	store := recordStore{dir: t.TempDir()}
	good := newDownloadRecord(hfdownloader.Job{Repo: "o/r"}, hfdownloader.Settings{CacheDir: "/c"})
	if err := store.save(good); err != nil {
		t.Fatal(err)
	}
	other := newDownloadRecord(hfdownloader.Job{Repo: "o/other"}, hfdownloader.Settings{CacheDir: "/c"})
	wrongVersion := *other
	wrongVersion.Version = 99
	data, _ := json.Marshal(wrongVersion)
	os.WriteFile(filepath.Join(store.dir, other.ID+".json"), data, 0o600)
	os.WriteFile(filepath.Join(store.dir, "abcdefabcdef.json"), []byte("{not json"), 0o600)
	misnamed, _ := json.Marshal(good)
	os.WriteFile(filepath.Join(store.dir, "000000000000.json"), misnamed, 0o600)
	os.WriteFile(filepath.Join(store.dir, "notes.txt"), []byte("x"), 0o600)

	recs, err := store.list()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].ID != good.ID {
		t.Fatalf("list = %d records, want only the valid one", len(recs))
	}
	if _, err := store.path("../../etc/passwd"); err == nil {
		t.Error("store accepted an id that is a path")
	}
	if (recordStore{dir: filepath.Join(t.TempDir(), "missing")}).load(good.ID) != nil {
		t.Error("load from a missing directory returned a record")
	}
}

func TestStripURLCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"http://proxy:3128":                 "http://proxy:3128",
		"http://user:pass@proxy:3128":       "http://proxy:3128",
		"socks5://user@proxy:1080":          "socks5://proxy:1080",
		"":                                  "",
		"http://us%zz:pa@proxy:1":           "http://proxy:1", // unparsable escape
		"https://u:p@proxy.example/path?q=": "https://proxy.example/path?q=",
	} {
		if got := stripURLCredentials(in); got != want {
			t.Errorf("stripURLCredentials(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildCommandString_StripsProxyCredentials(t *testing.T) {
	cmd := newDownloadCmd(context.Background(), &RootOpts{})
	got := buildCommandString(cmd, hfdownloader.Job{Repo: "o/r"}, hfdownloader.Settings{
		Proxy: &hfdownloader.ProxyConfig{URL: "http://user:secret@proxy:3128"}})
	if strings.Contains(got, "secret") || !strings.Contains(got, "--proxy http://proxy:3128") {
		t.Errorf("command = %q", got)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
