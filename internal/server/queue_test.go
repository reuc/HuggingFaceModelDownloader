// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

// fakeRunner returns a runFn that marks the job running, as runJob does,
// reports its repo on started and then blocks until release is closed.
func fakeRunner(m *JobManager, started chan<- string, release <-chan struct{}) func(*Job) {
	return func(j *Job) {
		m.mu.Lock()
		j.Status = JobStatusRunning
		m.mu.Unlock()
		started <- j.Repo
		<-release
	}
}

// queueFixture starts a manager with a fakeRunner and creates a download that
// starts running at once, followed by one queued job per repo in queued. It
// returns the queued jobs' IDs keyed by repo.
func queueFixture(t *testing.T, cfg Config, queued ...string) (m *JobManager, started chan string, release chan struct{}, ids map[string]string) {
	t.Helper()
	m = NewJobManager(cfg, nil)
	started = make(chan string, len(queued)+1)
	release = make(chan struct{})
	m.runFn = fakeRunner(m, started, release)
	if _, _, err := m.CreateJob(DownloadRequest{Repo: "owner/busy"}); err != nil {
		t.Fatal(err)
	}
	if repo := <-started; repo != "owner/busy" {
		t.Fatalf("first run %q, want owner/busy", repo)
	}
	ids = map[string]string{}
	for _, repo := range queued {
		j, _, err := m.CreateJob(DownloadRequest{Repo: repo})
		if err != nil {
			t.Fatal(err)
		}
		ids[repo] = j.ID
	}
	return m, started, release, ids
}

// runOrder releases the runner and returns the repos in the order they ran.
func runOrder(t *testing.T, m *JobManager, started chan string, release chan struct{}, n int) []string {
	t.Helper()
	close(release)
	var order []string
	for i := 0; i < n; i++ {
		select {
		case repo := <-started:
			order = append(order, repo)
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d queued jobs ran: %v", i, n, order)
		}
	}
	if !m.WaitAll(2 * time.Second) {
		t.Fatal("runs did not finish")
	}
	return order
}

// clientQueueOrder sorts the queued jobs by QueueSeq the way the web UI does.
func clientQueueOrder(m *JobManager) []string {
	var queued []*Job
	for _, j := range m.ListJobs() {
		if j.Status == JobStatusQueued {
			queued = append(queued, j)
		}
	}
	sort.Slice(queued, func(a, b int) bool { return queued[a].QueueSeq < queued[b].QueueSeq })
	repos := make([]string, len(queued))
	for i, j := range queued {
		repos[i] = j.Repo
	}
	return repos
}

func equalOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Moving a queued job to the top makes it the next download, without
// interrupting the one that is running; the rest keep their order. QueueSeq
// reflects the new order so clients sort the list the same way.
func TestMoveJobToTop_RunsNext(t *testing.T) {
	m, started, release, ids := queueFixture(t, Config{}, "owner/a", "owner/b", "owner/c")
	ch := m.Subscribe()

	res, snap := m.MoveJobToTop(ids["owner/c"])
	if res != MoveJobOK {
		t.Fatalf("MoveJobToTop = %v, want MoveJobOK", res)
	}
	if snap.ID != ids["owner/c"] || snap.Status != JobStatusQueued {
		t.Errorf("snapshot id=%q status=%q, want owner/c queued", snap.ID, snap.Status)
	}
	select {
	case j := <-ch:
		if j.ID != ids["owner/c"] {
			t.Errorf("update for %s, want owner/c", j.Repo)
		}
	default:
		t.Error("no job update sent for the moved job")
	}
	select {
	case repo := <-started:
		t.Fatalf("%s started while another download was running", repo)
	default:
	}

	want := []string{"owner/c", "owner/a", "owner/b"}
	if got := clientQueueOrder(m); !equalOrder(got, want) {
		t.Errorf("QueueSeq order = %v, want %v", got, want)
	}
	if got := runOrder(t, m, started, release, 3); !equalOrder(got, want) {
		t.Errorf("run order = %v, want %v", got, want)
	}
}

// Moving more than one job: the last one moved runs first.
func TestMoveJobToTop_Repeated(t *testing.T) {
	m, started, release, ids := queueFixture(t, Config{}, "owner/a", "owner/b", "owner/c", "owner/d")

	for _, repo := range []string{"owner/c", "owner/d", "owner/b"} {
		if res, _ := m.MoveJobToTop(ids[repo]); res != MoveJobOK {
			t.Fatalf("MoveJobToTop(%s) = %v, want MoveJobOK", repo, res)
		}
	}

	want := []string{"owner/b", "owner/d", "owner/c", "owner/a"}
	if got := clientQueueOrder(m); !equalOrder(got, want) {
		t.Errorf("QueueSeq order = %v, want %v", got, want)
	}
	if got := runOrder(t, m, started, release, 4); !equalOrder(got, want) {
		t.Errorf("run order = %v, want %v", got, want)
	}
}

// The job that is already next stays where it is, and no update is sent.
// Cancelled jobs left in the queue don't count as being ahead of it.
func TestMoveJobToTop_AlreadyNext(t *testing.T) {
	m, started, release, ids := queueFixture(t, Config{}, "owner/a", "owner/b", "owner/c")
	m.CancelJob(ids["owner/a"])
	before, _ := m.GetJob(ids["owner/b"])
	ch := m.Subscribe()

	res, snap := m.MoveJobToTop(ids["owner/b"])
	if res != MoveJobOK {
		t.Fatalf("MoveJobToTop = %v, want MoveJobOK", res)
	}
	if snap.QueueSeq != before.QueueSeq {
		t.Errorf("QueueSeq changed from %d to %d", before.QueueSeq, snap.QueueSeq)
	}
	select {
	case j := <-ch:
		t.Errorf("unexpected update for %s", j.Repo)
	default:
	}

	want := []string{"owner/b", "owner/c"}
	if got := runOrder(t, m, started, release, 2); !equalOrder(got, want) {
		t.Errorf("run order = %v, want %v", got, want)
	}
}

func TestMoveJobToTop_OnlyQueuedJobs(t *testing.T) {
	m := NewJobManager(Config{}, nil)
	m.runFn = func(*Job) {}

	if res, _ := m.MoveJobToTop("missing"); res != MoveJobNotFound {
		t.Errorf("missing job: %v, want MoveJobNotFound", res)
	}
	for _, st := range []JobStatus{JobStatusRunning, JobStatusPaused, JobStatusCompleted, JobStatusFailed, JobStatusCancelled} {
		id := "job-" + string(st)
		m.jobs[id] = &Job{ID: id, Repo: "owner/" + id, Status: st, QueueSeq: 7, CreatedAt: time.Now()}
		res, snap := m.MoveJobToTop(id)
		if res != MoveJobNotQueued || snap == nil || snap.Status != st {
			t.Errorf("%s: %v (%v), want MoveJobNotQueued", st, res, snap)
		}
		if j := m.jobs[id]; j.Status != st || j.QueueSeq != 7 {
			t.Errorf("%s: job changed to status %s, QueueSeq %d", st, j.Status, j.QueueSeq)
		}
	}
	if len(m.queue) != 0 {
		t.Errorf("queue = %v, want it untouched", m.queue)
	}
}

// The new order is saved, so a restart downloads the moved job next.
func TestMoveJobToTop_SurvivesRestart(t *testing.T) {
	cfg := persistConfig(t)
	m1, _, release, ids := queueFixture(t, cfg, "owner/a", "owner/b", "owner/c")
	// m1 is "killed" with owner/busy still running; releasing it at the end
	// makes it save once more, so wait for that before the temp dir goes.
	t.Cleanup(func() {
		m1.mu.Lock()
		for _, j := range m1.jobs {
			j.Status = JobStatusCancelled // don't run the rest during cleanup
		}
		m1.mu.Unlock()
		close(release)
		m1.WaitAll(2 * time.Second)
	})

	if res, _ := m1.MoveJobToTop(ids["owner/b"]); res != MoveJobOK {
		t.Fatalf("MoveJobToTop = %v, want MoveJobOK", res)
	}

	m2 := NewJobManager(cfg, nil)
	started := make(chan string, 4)
	m2.runFn = func(j *Job) { started <- j.Repo }
	m2.Start()
	var order []string
	for i := 0; i < 4; i++ {
		select {
		case repo := <-started:
			order = append(order, repo)
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d restored jobs ran: %v", i, order)
		}
	}
	m2.WaitAll(2 * time.Second)

	// The interrupted download resumes first, then the queue in its new order.
	want := []string{"owner/busy", "owner/b", "owner/a", "owner/c"}
	if !equalOrder(order, want) {
		t.Errorf("run order after restart = %v, want %v", order, want)
	}
}

func TestAPI_MoveJobToTop(t *testing.T) {
	srv := newTestServer()
	release := make(chan struct{})
	started := make(chan string, 4)
	srv.jobs.runFn = fakeRunner(srv.jobs, started, release)
	t.Cleanup(func() { close(release); srv.jobs.WaitAll(2 * time.Second) })

	busy, _, err := srv.jobs.CreateJob(DownloadRequest{Repo: "owner/busy"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, _, err := srv.jobs.CreateJob(DownloadRequest{Repo: "owner/a"}); err != nil {
		t.Fatal(err)
	}
	b, _, err := srv.jobs.CreateJob(DownloadRequest{Repo: "owner/b"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.registerAPIRoutes(mux)

	post := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		return w
	}

	w := post("/api/jobs/" + b.ID + "/move-to-top")
	if w.Code != http.StatusOK {
		t.Fatalf("move queued job: status %d, body %s", w.Code, w.Body)
	}
	var got Job
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != b.ID || got.Status != JobStatusQueued {
		t.Errorf("response job id=%q status=%q", got.ID, got.Status)
	}
	if want := []string{"owner/b", "owner/a"}; !equalOrder(clientQueueOrder(srv.jobs), want) {
		t.Errorf("queue order = %v, want %v", clientQueueOrder(srv.jobs), want)
	}

	if w := post("/api/jobs/" + busy.ID + "/move-to-top"); w.Code != http.StatusConflict {
		t.Errorf("move a running job: status %d, want 409", w.Code)
	}
	if w := post("/api/jobs/nope/move-to-top"); w.Code != http.StatusNotFound {
		t.Errorf("move a missing job: status %d, want 404", w.Code)
	}
}
