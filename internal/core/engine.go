package core

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

//go:embed plugin/awflex.py
var callbackPlugin []byte

// group collects hosts waiting to share one ansible run.
type group struct {
	key   string
	req   Request
	hosts []string
	first time.Time // when the first waiting host arrived; the batch window counts from here
}

// batch is one ansible-playbook process.
type batch struct {
	id           string
	hosts        []string
	req          Request
	forks        int
	started      time.Time
	events       string
	playbookPath string // absolute playbook outside the repo; doctor only
	cancel       context.CancelFunc
	userCanceled bool
}

// Engine queues host requests, merges them into batches and runs ansible
// within a global fork budget.
type Engine struct {
	cfg       Config
	st        *Store
	git       *Git
	pluginDir string

	ctx     context.Context
	stopAll context.CancelFunc
	wg      sync.WaitGroup
	wake    chan struct{}

	mu         sync.Mutex
	groups     map[string]*group
	running    map[string]*batch
	forksInUse int
	stopping   bool
	finished   map[string]int // completed runs by final status since start
	hooks      int
}

func NewEngine(cfg Config, st *Store, git *Git) (*Engine, error) {
	e := &Engine{
		cfg: cfg, st: st, git: git,
		pluginDir: filepath.Join(cfg.TmpDir, "plugins"),
		wake:      make(chan struct{}, 1),
		groups:    map[string]*group{},
		running:   map[string]*batch{},
		finished:  map[string]int{},
	}
	e.ctx, e.stopAll = context.WithCancel(context.Background())
	// Scratch from a previous process holds nothing worth keeping.
	os.RemoveAll(cfg.TmpDir)
	if err := os.MkdirAll(e.pluginDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(e.pluginDir, "awflex.py"), callbackPlugin, 0o600); err != nil {
		return nil, err
	}
	e.recover()
	return e, nil
}

// recover rebuilds the queue from disk. A record still queued or running was
// accepted but never finished, because awflex or its node died; it runs again.
func (e *Engine) recover() {
	queued, gaveUp := 0, 0
	for _, r := range e.st.ListRuns("") {
		if isFinal(r.Status) {
			continue
		}
		if r.Status == StRunning && r.Attempts >= e.cfg.MaxAttempts {
			now := time.Now()
			e.st.Mutate(r.Host, func(r *Run) {
				r.Status, r.FinishedAt = StError, &now
				r.Error = fmt.Sprintf("gave up: ansible was started %d times for this request and never reported the host as finished", r.Attempts)
			})
			gaveUp++
			continue
		}
		if r.Status == StRunning {
			e.st.Mutate(r.Host, func(r *Run) { r.Status, r.StartedAt, r.BatchID = StQueued, nil, "" })
		}
		e.addToGroupLocked(r.Host, r.Request, r.QueuedAt)
		queued++
	}
	if queued+gaveUp > 0 {
		log.Printf("recovered queue from disk: %d unfinished runs re-queued, %d given up after %d attempts", queued, gaveUp, e.cfg.MaxAttempts)
	}
	e.st.gcBatchLogs(map[string]bool{})
}

func (e *Engine) Start() {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-e.wake:
			case <-t.C:
			}
			e.dispatch()
		}
	}()
}

// Stop kills running ansible processes and puts their unfinished hosts back
// to queued on disk, so the next start picks them up.
func (e *Engine) Stop() {
	e.mu.Lock()
	e.stopping = true
	e.mu.Unlock()
	e.stopAll()
	e.wg.Wait()
}

func (e *Engine) kick() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) addToGroupLocked(host string, req Request, first time.Time) {
	g := e.groups[req.Group]
	if g == nil {
		g = &group{key: req.Group, req: req, first: first}
		e.groups[req.Group] = g
	}
	if first.Before(g.first) {
		g.first = first
	}
	g.hosts = append(g.hosts, host)
}

func (e *Engine) removeFromGroupLocked(key, host string) {
	g := e.groups[key]
	if g == nil {
		return
	}
	for i, h := range g.hosts {
		if h == host {
			g.hosts = append(g.hosts[:i], g.hosts[i+1:]...)
			break
		}
	}
	if len(g.hosts) == 0 {
		delete(e.groups, key)
	}
}

// Submit queues req for each host and returns the resulting records.
func (e *Engine) Submit(hosts []string, req Request) []*Run {
	req.Group = req.groupKey()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Run, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, e.enqueueLocked(h, req))
	}
	if req.Source == "hook" {
		e.hooks += len(hosts)
	}
	e.kick()
	return out
}

func (e *Engine) enqueueLocked(host string, req Request) *Run {
	if cur := e.st.GetRun(host); cur != nil {
		switch cur.Status {
		case StRunning:
			// One ansible run per host at a time. The newest request waits and
			// starts as soon as the current run ends.
			return e.st.Mutate(host, func(r *Run) { r.Next = &req })
		case StQueued:
			if cur.Group == req.Group {
				return cur // same request already waiting
			}
			e.removeFromGroupLocked(cur.Group, host) // newest request wins
		}
	}
	return e.setQueuedLocked(host, req, 0)
}

func (e *Engine) setQueuedLocked(host string, req Request, attempts int) *Run {
	now := time.Now()
	r := &Run{ID: newID(), Host: host, Status: StQueued, Attempts: attempts, Request: req, QueuedAt: now}
	if err := e.st.SaveRun(r); err != nil {
		log.Printf("persist %s: %v", host, err)
	}
	e.addToGroupLocked(host, req, now)
	return r
}

// dispatch starts every batch that is due and fits the fork budget.
func (e *Engine) dispatch() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopping {
		return
	}
	now := time.Now()
	for {
		// Oldest group whose window has closed, or that is already full.
		var g *group
		for _, c := range e.groups {
			due := len(c.hosts) >= e.cfg.MaxBatch || now.Sub(c.first) >= e.cfg.BatchWindow
			if due && (g == nil || c.first.Before(g.first)) {
				g = c
			}
		}
		if g == nil {
			return
		}
		n := min(len(g.hosts), e.cfg.MaxBatch)
		forks := min(n, e.cfg.Forks, e.cfg.MaxForks)
		if e.forksInUse > 0 && e.forksInUse+forks > e.cfg.MaxForks {
			return // budget full; hosts keep collecting until a run ends
		}
		b := &batch{id: newID(), hosts: append([]string{}, g.hosts[:n]...), req: g.req, forks: forks, started: now}
		g.hosts = g.hosts[n:]
		if len(g.hosts) == 0 {
			delete(e.groups, g.key)
		}
		b.events = filepath.Join(e.cfg.TmpDir, "run-"+b.id, "events.jsonl")
		for _, h := range b.hosts {
			e.st.Mutate(h, func(r *Run) {
				r.Status, r.StartedAt, r.BatchID = StRunning, &now, b.id
				r.Attempts++
			})
		}
		e.forksInUse += forks
		e.running[b.id] = b
		e.wg.Add(1)
		go e.runBatch(b)
	}
}

func (e *Engine) runBatch(b *batch) {
	defer e.wg.Done()
	ctx, cancel := context.WithTimeout(e.ctx, e.cfg.Timeout)
	e.mu.Lock()
	b.cancel = cancel
	e.mu.Unlock()
	log.Printf("batch %s start: %d hosts ref=%s playbook=%s forks=%d", b.id, len(b.hosts), b.req.Ref, b.req.Playbook, b.forks)
	res := e.execute(ctx, b)
	cancel()
	e.finish(b, res)
}

// finish records the outcome per host. A host without a final ansible summary
// did not finish; it is re-queued when the cause was a crash or a shutdown.
func (e *Engine) finish(b *batch, res execResult) {
	e.mu.Lock()
	now := time.Now()
	counts := map[string]int{}
	for _, h := range b.hosts {
		cur := e.st.GetRun(h)
		if cur == nil || cur.BatchID != b.id {
			continue // forgotten while running
		}
		hr := res.hosts[h]
		done := hr != nil && hr.stats != nil
		if !done && !b.userCanceled {
			if e.stopping {
				// Shutting down is not the request's fault: do not count the attempt.
				e.st.Mutate(h, func(r *Run) {
					r.Status, r.StartedAt, r.BatchID = StQueued, nil, ""
					r.Attempts--
				})
				counts["requeued"]++
				continue
			}
			if res.crashed && cur.Attempts < e.cfg.MaxAttempts {
				req, attempts := cur.Request, cur.Attempts
				if cur.Next != nil {
					req, attempts = *cur.Next, 0
				}
				e.setQueuedLocked(h, req, attempts)
				counts["requeued"]++
				continue
			}
		}
		status, msg := classify(hr, res, b.userCanceled)
		var next *Request
		r := e.st.Mutate(h, func(r *Run) {
			r.Status, r.FinishedAt, r.Commit, r.Error = status, &now, res.commit, msg
			r.Stats, r.FailedTasks, r.Hint = nil, nil, ""
			if hr != nil {
				r.Stats, r.FailedTasks = hr.stats, hr.failedTasks()
			}
			if status != StSuccessful && status != StCanceled {
				r.Hint = runHint(msg, hr)
			}
			next, r.Next = r.Next, nil
		})
		e.st.WriteLog(h, renderLog(r, hr))
		e.finished[status]++
		counts[status]++
		if next != nil {
			e.enqueueLocked(h, *next)
		}
	}
	e.forksInUse -= b.forks
	delete(e.running, b.id)
	keep := map[string]bool{}
	for id := range e.running {
		keep[id] = true
	}
	e.mu.Unlock()

	log.Printf("batch %s done in %s: %v", b.id, time.Since(b.started).Round(time.Millisecond), counts)
	e.st.gcBatchLogs(keep)
	e.kick()
}

// Cancel removes a queued host from the queue.
func (e *Engine) Cancel(host string) (*Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := e.st.GetRun(host)
	switch {
	case cur == nil:
		return nil, os.ErrNotExist
	case cur.Status == StRunning:
		return cur, fmt.Errorf("host is running in batch %s with other hosts; cancel the batch with POST /api/v1/batches/%s/cancel", cur.BatchID, cur.BatchID)
	case cur.Status != StQueued:
		return cur, fmt.Errorf("nothing to cancel: last run is %s", cur.Status)
	}
	e.removeFromGroupLocked(cur.Group, host)
	now := time.Now()
	return e.st.Mutate(host, func(r *Run) { r.Status, r.FinishedAt = StCanceled, &now }), nil
}

// CancelBatch kills a running ansible process. Hosts it had not finished become canceled.
func (e *Engine) CancelBatch(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.running[id]
	if b == nil || b.cancel == nil {
		return os.ErrNotExist
	}
	b.userCanceled = true
	b.cancel()
	return nil
}

// Forget deletes a host's record, for decommissioned nodes.
func (e *Engine) Forget(host string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := e.st.GetRun(host)
	if cur == nil {
		return os.ErrNotExist
	}
	if cur.Status == StQueued {
		e.removeFromGroupLocked(cur.Group, host)
	}
	e.st.DeleteRun(host)
	return nil
}

// LiveLog renders what a still-running host has done so far.
func (e *Engine) LiveLog(r *Run) []byte {
	e.mu.Lock()
	b := e.running[r.BatchID]
	e.mu.Unlock()
	if b == nil {
		return nil
	}
	return renderLog(r, parseEvents(b.events)[r.Host])
}

type BatchInfo struct {
	ID        string    `json:"id"`
	Hosts     int       `json:"hosts"`
	Ref       string    `json:"ref"`
	Playbook  string    `json:"playbook"`
	Forks     int       `json:"forks"`
	StartedAt time.Time `json:"started_at"`
}

type WaitingInfo struct {
	Hosts      int     `json:"hosts"`
	Ref        string  `json:"ref"`
	Playbook   string  `json:"playbook"`
	WaitingSec float64 `json:"waiting_seconds"`
}

type Status struct {
	Version     string         `json:"version"`
	Queued      int            `json:"queued"`
	Running     int            `json:"running"`
	ForksInUse  int            `json:"forks_in_use"`
	MaxForks    int            `json:"max_forks"`
	Batches     []BatchInfo    `json:"batches"`
	Waiting     []WaitingInfo  `json:"waiting"`
	Hosts       map[string]int `json:"hosts"`    // host records by current status
	Finished    map[string]int `json:"finished"` // runs completed since this process started
	HookCalls   int            `json:"hook_calls"`
	BatchWindow float64        `json:"batch_window_seconds"`
}

func (e *Engine) Status() Status {
	s := Status{Version: Version, MaxForks: e.cfg.MaxForks, Batches: []BatchInfo{}, Waiting: []WaitingInfo{},
		Hosts: map[string]int{}, Finished: map[string]int{}, BatchWindow: e.cfg.BatchWindow.Seconds()}
	for _, r := range e.st.ListRuns("") {
		s.Hosts[r.Status]++
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s.ForksInUse, s.HookCalls = e.forksInUse, e.hooks
	for k, v := range e.finished {
		s.Finished[k] = v
	}
	for _, b := range e.running {
		s.Running += len(b.hosts)
		s.Batches = append(s.Batches, BatchInfo{b.id, len(b.hosts), b.req.Ref, b.req.Playbook, b.forks, b.started})
	}
	for _, g := range e.groups {
		s.Queued += len(g.hosts)
		s.Waiting = append(s.Waiting, WaitingInfo{len(g.hosts), g.req.Ref, g.req.Playbook, time.Since(g.first).Seconds()})
	}
	sort.Slice(s.Batches, func(i, j int) bool { return s.Batches[i].ID < s.Batches[j].ID })
	sort.Slice(s.Waiting, func(i, j int) bool { return s.Waiting[i].WaitingSec > s.Waiting[j].WaitingSec })
	return s
}
