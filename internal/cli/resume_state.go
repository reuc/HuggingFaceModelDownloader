// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/reuc/HuggingFaceModelDownloader/pkg/hfdownloader"
)

// Every CLI download is recorded while it runs, so one that fails or is
// interrupted can be continued with `hfdownloader resume` without retyping
// the command. Each download has its own small JSON file in
// ~/.config/hfdownloader/downloads/, named by an ID derived from what is
// downloaded and where to, so running the same download again (by either
// command) updates the same record, and a run that completes deletes it.
// One file per download means concurrent CLI processes never rewrite each
// other's state.
//
// Records never hold the access token or proxy credentials: a resume reads
// those from --token, HF_TOKEN and the config file like any download.

const (
	recordVersion = 1

	statusRunning     = "running"
	statusFailed      = "failed"
	statusInterrupted = "interrupted"

	// A record not refreshed for staleAfter belongs to a process that died
	// without recording why (killed, crashed, power loss).
	staleAfter = 2 * time.Minute

	// maxRecordedError caps the stored error text (a multi-file failure
	// joins one message per file).
	maxRecordedError = 4000
)

// heartbeatInterval is how often a running download refreshes its record
// (a variable so tests can shorten it).
var heartbeatInterval = 30 * time.Second

// downloadRecord is one resumable download on disk.
type downloadRecord struct {
	Version   int            `json:"version"`
	ID        string         `json:"id"`
	Status    string         `json:"status"`
	Error     string         `json:"error,omitempty"`
	Command   string         `json:"command,omitempty"`
	Job       recordJob      `json:"job"`
	Settings  recordSettings `json:"settings"`
	Progress  recordProgress `json:"progress"`
	Attempts  int            `json:"attempts"`
	PID       int            `json:"pid,omitempty"`
	Host      string         `json:"host,omitempty"`
	StartedAt time.Time      `json:"started_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// recordJob is what is downloaded.
type recordJob struct {
	Repo               string   `json:"repo"`
	Dataset            bool     `json:"dataset,omitempty"`
	Revision           string   `json:"revision"`
	Filters            []string `json:"filters,omitempty"`
	Excludes           []string `json:"excludes,omitempty"`
	Shards             string   `json:"shards,omitempty"`
	ExactMatch         bool     `json:"exact_match,omitempty"`
	AppendFilterSubdir bool     `json:"append_filter_subdir,omitempty"`
}

// recordSettings are the resolved download settings, minus secrets. Paths are
// absolute so a resume from another directory writes to the same place.
type recordSettings struct {
	CacheDir           string `json:"cache_dir,omitempty"`
	OutputDir          string `json:"output_dir,omitempty"`
	StaleTimeout       string `json:"stale_timeout,omitempty"`
	Concurrency        int    `json:"connections,omitempty"`
	MaxActiveDownloads int    `json:"max_active,omitempty"`
	MultipartThreshold string `json:"multipart_threshold,omitempty"`
	Verify             string `json:"verify,omitempty"`
	Retries            int    `json:"retries,omitempty"`
	BackoffInitial     string `json:"backoff_initial,omitempty"`
	BackoffMax         string `json:"backoff_max,omitempty"`
	StallTimeout       string `json:"stall_timeout,omitempty"`
	LinkMode           string `json:"link_mode,omitempty"`
	Endpoint           string `json:"endpoint,omitempty"`
	NoManifest         bool   `json:"no_manifest,omitempty"`
	NoFriendlyView     bool   `json:"no_friendly,omitempty"`
	Flatten            bool   `json:"flatten,omitempty"`
	ProxyURL           string `json:"proxy_url,omitempty"`
	NoEnvProxy         bool   `json:"no_env_proxy,omitempty"`
}

// recordProgress is how far the last attempt got.
type recordProgress struct {
	FilesTotal int   `json:"files_total"`
	FilesDone  int   `json:"files_done"`
	BytesTotal int64 `json:"bytes_total"`
	BytesDone  int64 `json:"bytes_done"`
}

// newDownloadRecord describes job and cfg as a record (status running).
func newDownloadRecord(job hfdownloader.Job, cfg hfdownloader.Settings) *downloadRecord {
	rev := job.Revision
	if rev == "" {
		rev = "main"
	}
	rj := recordJob{
		Repo:               job.Repo,
		Dataset:            job.IsDataset,
		Revision:           rev,
		Filters:            append([]string(nil), job.Filters...),
		Excludes:           append([]string(nil), job.Excludes...),
		Shards:             job.Shards,
		ExactMatch:         job.ExactMatch,
		AppendFilterSubdir: job.AppendFilterSubdir,
	}

	// Same storage choice as hfdownloader.Download: the HF cache unless only
	// a flat output directory is set.
	rs := recordSettings{
		StaleTimeout:       cfg.StaleTimeout,
		Concurrency:        cfg.Concurrency,
		MaxActiveDownloads: cfg.MaxActiveDownloads,
		MultipartThreshold: cfg.MultipartThreshold,
		Verify:             cfg.Verify,
		Retries:            cfg.Retries,
		BackoffInitial:     cfg.BackoffInitial,
		BackoffMax:         cfg.BackoffMax,
		StallTimeout:       cfg.StallTimeout,
		LinkMode:           cfg.LinkMode,
		Endpoint:           cfg.Endpoint,
		NoManifest:         cfg.NoManifest,
		NoFriendlyView:     cfg.NoFriendlyView,
		Flatten:            cfg.Flatten,
	}
	if cfg.CacheDir != "" || cfg.OutputDir == "" {
		dir := cfg.CacheDir
		if dir == "" {
			dir = hfdownloader.DefaultCacheDir()
		}
		rs.CacheDir = absPath(dir)
	} else {
		rs.OutputDir = absPath(cfg.OutputDir)
	}
	if cfg.Proxy != nil {
		rs.ProxyURL = stripURLCredentials(cfg.Proxy.URL)
		rs.NoEnvProxy = cfg.Proxy.NoEnvProxy
	}

	host, _ := os.Hostname()
	now := time.Now().UTC()
	return &downloadRecord{
		Version:   recordVersion,
		ID:        recordID(rj, rs),
		Status:    statusRunning,
		Command:   cfg.Command,
		Job:       rj,
		Settings:  rs,
		PID:       os.Getpid(),
		Host:      host,
		StartedAt: now,
		UpdatedAt: now,
	}
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// stripURLCredentials removes user:password from a URL such as a proxy
// http://user:pass@host:3128, so it can be stored or shown. Credentials are
// dropped rather than masked: a masked password would be sent on reuse.
func stripURLCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// Not a URL we can take apart; keep only what follows a userinfo @.
		if at := strings.LastIndex(raw, "@"); at >= 0 {
			if scheme := strings.Index(raw, "://"); scheme >= 0 && scheme < at {
				return raw[:scheme+3] + raw[at+1:]
			}
			return raw[at+1:]
		}
		return raw
	}
	if u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// recordID identifies a download by what it fetches and where it puts it.
// Tuning (connections, retries, ...) and post-processing choices (flatten,
// manifest) are left out: changing them between attempts still continues the
// same download from the same partial files.
func recordID(j recordJob, s recordSettings) string {
	sorted := func(v []string) []string {
		out := append([]string(nil), v...)
		sort.Strings(out)
		return out
	}
	key := struct {
		Repo, Revision, Shards, CacheDir, OutputDir string
		Dataset, Exact, Subdir                      bool
		Filters, Excludes                           []string
	}{j.Repo, j.Revision, j.Shards, s.CacheDir, s.OutputDir, j.Dataset, j.ExactMatch, j.AppendFilterSubdir, sorted(j.Filters), sorted(j.Excludes)}
	b, _ := json.Marshal(key)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// job rebuilds the download job.
func (r *downloadRecord) job() hfdownloader.Job {
	return hfdownloader.Job{
		Repo:               r.Job.Repo,
		IsDataset:          r.Job.Dataset,
		Revision:           r.Job.Revision,
		Filters:            append([]string(nil), r.Job.Filters...),
		Excludes:           append([]string(nil), r.Job.Excludes...),
		Shards:             r.Job.Shards,
		ExactMatch:         r.Job.ExactMatch,
		AppendFilterSubdir: r.Job.AppendFilterSubdir,
	}
}

// settings rebuilds the download settings, without token or proxy
// credentials (the caller adds those).
func (r *downloadRecord) settings() hfdownloader.Settings {
	s := r.Settings
	cfg := hfdownloader.Settings{
		CacheDir:           s.CacheDir,
		OutputDir:          s.OutputDir,
		StaleTimeout:       s.StaleTimeout,
		Concurrency:        s.Concurrency,
		MaxActiveDownloads: s.MaxActiveDownloads,
		MultipartThreshold: s.MultipartThreshold,
		Verify:             s.Verify,
		Retries:            s.Retries,
		BackoffInitial:     s.BackoffInitial,
		BackoffMax:         s.BackoffMax,
		StallTimeout:       s.StallTimeout,
		LinkMode:           s.LinkMode,
		Endpoint:           s.Endpoint,
		NoManifest:         s.NoManifest,
		NoFriendlyView:     s.NoFriendlyView,
		Flatten:            s.Flatten,
		Command:            r.Command,
	}
	if s.ProxyURL != "" || s.NoEnvProxy {
		cfg.Proxy = &hfdownloader.ProxyConfig{URL: s.ProxyURL, NoEnvProxy: s.NoEnvProxy}
	}
	return cfg
}

// state is the record's status as of now: a "running" record whose process
// is gone, or that stopped refreshing, was interrupted.
func (r *downloadRecord) state(now time.Time) string {
	if r.Status != statusRunning {
		return r.Status
	}
	if now.Sub(r.UpdatedAt) > staleAfter {
		return statusInterrupted
	}
	if host, _ := os.Hostname(); r.Host == host && !processAlive(r.PID) {
		return statusInterrupted
	}
	// Recently refreshed by a live process here, or by a process on another
	// machine sharing this config directory.
	return statusRunning
}

// label names the download for people: repo[@revision] plus its selection.
func (r *downloadRecord) label() string {
	s := r.Job.Repo
	if r.Job.Revision != "" && r.Job.Revision != "main" {
		s += "@" + r.Job.Revision
	}
	var sel []string
	if len(r.Job.Filters) > 0 {
		sel = append(sel, strings.Join(r.Job.Filters, ","))
	}
	if len(r.Job.Excludes) > 0 {
		sel = append(sel, "-"+strings.Join(r.Job.Excludes, ",-"))
	}
	if r.Job.Shards != "" {
		sel = append(sel, "shards "+r.Job.Shards)
	}
	if len(sel) > 0 {
		s += " [" + strings.Join(sel, " ") + "]"
	}
	if r.Job.Dataset {
		s += " (dataset)"
	}
	return s
}

// processAlive reports whether a process with this PID exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// FindProcess opens the process on Windows and fails if none has
		// this PID.
		_ = p.Release()
		return true
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// recordStore is the directory of download records.
type recordStore struct{ dir string }

func defaultRecordStore() recordStore {
	home, _ := os.UserHomeDir()
	return recordStore{dir: filepath.Join(home, ".config", "hfdownloader", "downloads")}
}

var recordIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

func (s recordStore) path(id string) (string, error) {
	if !recordIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid download id %q", id)
	}
	return filepath.Join(s.dir, id+".json"), nil
}

// load returns the record with this ID, or nil if there is none (or it can't
// be read: an unreadable record is no reason to fail a download).
func (s recordStore) load(id string) *downloadRecord {
	p, err := s.path(id)
	if err != nil {
		return nil
	}
	return readRecord(p)
}

func readRecord(path string) *downloadRecord {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r downloadRecord
	if json.Unmarshal(data, &r) != nil || r.Version != recordVersion || !recordIDPattern.MatchString(r.ID) || r.Job.Repo == "" {
		return nil
	}
	if filepath.Base(path) != r.ID+".json" {
		return nil
	}
	return &r
}

// list returns every readable record, most recently active first.
func (s recordStore) list() ([]*downloadRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*downloadRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if r := readRecord(filepath.Join(s.dir, e.Name())); r != nil {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// save writes r atomically (temp file + rename), so a crash mid-write leaves
// the previous record rather than a truncated one.
func (s recordStore) save(r *downloadRecord) error {
	p, err := s.path(r.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, r.ID+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (s recordStore) remove(id string) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// downloadTracker keeps a download's record current while it runs.
//
// The record is first written once the repo has been scanned (its first
// plan_item event), so a download that fails before anything could be
// fetched — a mistyped repo, a revision that doesn't exist — leaves nothing
// to resume. A download that already had a record (a previous failed attempt)
// is marked running immediately.
type downloadTracker struct {
	store recordStore

	mu      sync.Mutex
	rec     *downloadRecord
	saved   bool  // a record for this download exists on disk
	saveErr error // result of the last write (nil once one succeeds again)
	sizes   map[string]int64
	got     map[string]int64
	done    map[string]bool

	stop chan struct{}
	wg   sync.WaitGroup
}

func newDownloadTracker(store recordStore, job hfdownloader.Job, cfg hfdownloader.Settings) *downloadTracker {
	t := &downloadTracker{
		store: store,
		rec:   newDownloadRecord(job, cfg),
		sizes: map[string]int64{},
		got:   map[string]int64{},
		done:  map[string]bool{},
		stop:  make(chan struct{}),
	}
	t.rec.Attempts = 1
	if prev := store.load(t.rec.ID); prev != nil {
		t.rec.StartedAt = prev.StartedAt
		t.rec.Attempts = prev.Attempts + 1
		t.rec.Progress = prev.Progress // until this attempt's plan replaces it
		t.saveLocked()
	}
	t.wg.Add(1)
	go t.heartbeat()
	return t
}

// id is the download's ID, as passed to `hfdownloader resume`.
func (t *downloadTracker) id() string { return t.rec.ID }

func (t *downloadTracker) heartbeat() {
	defer t.wg.Done()
	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			t.mu.Lock()
			if t.saved {
				t.saveLocked()
			}
			t.mu.Unlock()
		}
	}
}

// wrap returns a progress handler that updates the record, then calls next.
func (t *downloadTracker) wrap(next hfdownloader.ProgressFunc) hfdownloader.ProgressFunc {
	planned := false
	return func(ev hfdownloader.ProgressEvent) {
		t.mu.Lock()
		switch ev.Event {
		case "plan_item":
			if !planned {
				planned = true
				t.rec.Progress = recordProgress{}
			}
			if _, dup := t.sizes[ev.Path]; !dup {
				t.sizes[ev.Path] = ev.Total
				t.rec.Progress.FilesTotal++
				t.rec.Progress.BytesTotal += ev.Total
			}
			if !t.saved {
				t.saveLocked()
			}
		case "file_progress":
			t.got[ev.Path] = ev.Downloaded
		case "file_done":
			t.done[ev.Path] = true
		}
		t.mu.Unlock()
		if next != nil {
			next(ev)
		}
	}
}

// saveLocked refreshes progress and the timestamp and writes the record.
// Must be called with t.mu held.
func (t *downloadTracker) saveLocked() {
	if len(t.sizes) > 0 {
		var files int
		var bytes int64
		for path, size := range t.sizes {
			switch {
			case t.done[path]:
				files++
				bytes += size
			case t.got[path] > size && size > 0:
				bytes += size
			default:
				bytes += t.got[path]
			}
		}
		t.rec.Progress.FilesDone = files
		t.rec.Progress.BytesDone = bytes
	}
	t.rec.UpdatedAt = time.Now().UTC()
	t.saveErr = t.store.save(t.rec)
	if t.saveErr == nil {
		t.saved = true
	}
}

// finish records how the download ended: a completed download's record is
// deleted; a failed or interrupted one is kept with the reason. It reports
// whether the download can be resumed with `hfdownloader resume`.
func (t *downloadTracker) finish(ctx context.Context, err error) bool {
	close(t.stop)
	t.wg.Wait()

	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		if t.saved {
			if rmErr := t.store.remove(t.rec.ID); rmErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not remove the resume record of this completed download: %v\n", rmErr)
			}
		}
		return false
	}
	if !t.saved {
		// Failed before the repo was scanned: nothing to resume. (A write
		// that failed would have been for a record of the plan.)
		t.warnUnrecorded()
		return false
	}
	t.rec.Status = statusFailed
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		t.rec.Status = statusInterrupted
	}
	msg := err.Error()
	if len(msg) > maxRecordedError {
		msg = msg[:maxRecordedError] + "…"
	}
	t.rec.Error = msg
	t.rec.PID = 0
	t.saveLocked()
	if t.saveErr != nil {
		t.warnUnrecorded()
		return false
	}
	return true
}

func (t *downloadTracker) warnUnrecorded() {
	if t.saveErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record this download in %s, so `hfdownloader resume` won't list it (run the same command again to continue it): %v\n", t.store.dir, t.saveErr)
	}
}

// resumableError is a download error whose partial data was kept and
// recorded; Execute prints how to resume it after the error.
type resumableError struct {
	err error
	id  string
}

func (e *resumableError) Error() string { return e.err.Error() }
func (e *resumableError) Unwrap() error { return e.err }
