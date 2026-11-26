// Package operation runs long admin operations (git clones, image builds)
// in the background and keeps their log, so that API clients can start one
// and follow it.
//
// Only one operation runs at a time: they all work on challenge_dir and on
// the Ansible hosts, and two clones, or a pull and a build of the same
// repository, must not interleave. Operations live in memory; the most
// recent ones are kept after they finish.
package operation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Status of an operation.
const (
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusWarning = "warning" // finished, but some items failed
	StatusFailed  = "failed"
	// StatusCancelled: stopped by Cancel (or Shutdown) before it finished
	StatusCancelled = "cancelled"
)

// Log levels.
const (
	LevelDebug   = "debug"
	LevelInfo    = "info"
	LevelSuccess = "success"
	LevelWarning = "warning"
	LevelError   = "error"
)

const (
	maxKept     = 50
	maxLogLines = 20000
)

// ErrBusy is returned by Start while another operation runs.
var ErrBusy = errors.New("another operation is running")

// Line is one log line.
type Line struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// Operation is a background task and its log.
type Operation struct {
	mu         sync.Mutex
	ID         string
	Kind       string
	Target     string
	Status     string
	CreatedAt  time.Time
	FinishedAt *time.Time
	Result     map[string]interface{}
	lines      []Line
	truncated  bool
	cancel     context.CancelFunc
}

// Logger writes to an operation's log (also what the repository and build
// code log to).
type Logger interface {
	Debugf(format string, args ...interface{})
	Infof(format string, args ...interface{})
	Successf(format string, args ...interface{})
	Warnf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

// Snapshot is a copy of an operation's state, safe to serialise.
type Snapshot struct {
	ID         string
	Kind       string
	Target     string
	Status     string
	CreatedAt  time.Time
	FinishedAt *time.Time
	Result     map[string]interface{}
	Lines      []Line // from the requested offset
	LineCount  int    // total number of lines
}

func (o *Operation) add(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, part := range strings.Split(msg, "\n") {
		if len(o.lines) >= maxLogLines {
			if !o.truncated {
				o.truncated = true
				o.lines = append(o.lines, Line{Time: time.Now(), Level: LevelWarning, Message: "Log truncated"})
			}
			return
		}
		o.lines = append(o.lines, Line{Time: time.Now(), Level: level, Message: part})
	}
}

func (o *Operation) Debugf(f string, a ...interface{})   { o.add(LevelDebug, f, a...) }
func (o *Operation) Infof(f string, a ...interface{})    { o.add(LevelInfo, f, a...) }
func (o *Operation) Successf(f string, a ...interface{}) { o.add(LevelSuccess, f, a...) }
func (o *Operation) Warnf(f string, a ...interface{})    { o.add(LevelWarning, f, a...) }
func (o *Operation) Errorf(f string, a ...interface{})   { o.add(LevelError, f, a...) }

// Errors counts the error lines written so far.
func (o *Operation) Errors() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, l := range o.lines {
		if l.Level == LevelError {
			n++
		}
	}
	return n
}

// Snapshot copies the operation, with the log lines from offset on.
func (o *Operation) Snapshot(offset int) Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	if offset < 0 || offset > len(o.lines) {
		offset = len(o.lines)
	}
	return Snapshot{
		ID: o.ID, Kind: o.Kind, Target: o.Target, Status: o.Status,
		CreatedAt: o.CreatedAt, FinishedAt: o.FinishedAt, Result: o.Result,
		Lines: append([]Line(nil), o.lines[offset:]...), LineCount: len(o.lines),
	}
}

// Func is an operation body. The returned result is attached to the
// operation; a non-nil error fails it, error lines in the log make it a
// warning.
type Func func(ctx context.Context, log Logger) (map[string]interface{}, error)

// Manager starts and keeps operations.
type Manager struct {
	mu      sync.Mutex
	ops     []*Operation // oldest first
	running *Operation
	seq     int
	wg      sync.WaitGroup
}

func NewManager() *Manager { return &Manager{} }

// Start runs fn in the background, unless an operation is already running.
func (m *Manager) Start(kind, target string, fn Func) (*Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running != nil {
		return m.running, ErrBusy
	}
	m.seq++
	ctx, cancel := context.WithCancel(context.Background())
	op := &Operation{
		ID:        fmt.Sprintf("%d-%d", time.Now().Unix(), m.seq),
		Kind:      kind,
		Target:    target,
		Status:    StatusRunning,
		CreatedAt: time.Now(),
		cancel:    cancel,
	}
	m.ops = append(m.ops, op)
	if len(m.ops) > maxKept {
		m.ops = m.ops[len(m.ops)-maxKept:]
	}
	m.running = op
	m.wg.Add(1)
	go m.run(ctx, op, fn)
	return op, nil
}

func (m *Manager) run(ctx context.Context, op *Operation, fn Func) {
	defer m.wg.Done()
	defer op.cancel()
	result, err := safeCall(ctx, op, fn)
	status := StatusSuccess
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		if err != nil {
			op.Errorf("%v", err)
		}
		op.Warnf("Cancelled")
		status = StatusCancelled
	case err != nil:
		op.Errorf("%v", err)
		status = StatusFailed
	case op.Errors() > 0:
		status = StatusWarning
	}
	// Finished and no longer running at once: a client that saw it finish
	// got "another operation is running" when it started the next one
	now := time.Now()
	m.mu.Lock()
	op.mu.Lock()
	op.Status, op.FinishedAt, op.Result = status, &now, result
	op.mu.Unlock()
	if m.running == op {
		m.running = nil
	}
	m.mu.Unlock()
	zap.S().Infof("Operation %s (%s %s) finished: %s", op.ID, op.Kind, op.Target, status)
}

func safeCall(ctx context.Context, op *Operation, fn Func) (result map[string]interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			zap.S().Errorf("Operation %s panicked: %v", op.ID, r)
			err = fmt.Errorf("internal error: %v", r)
		}
	}()
	return fn(ctx, op)
}

// Get returns an operation by ID.
func (m *Manager) Get(id string) (*Operation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range m.ops {
		if op.ID == id {
			return op, true
		}
	}
	return nil, false
}

// List returns the kept operations, the most recent first.
func (m *Manager) List() []*Operation {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Operation, 0, len(m.ops))
	for i := len(m.ops) - 1; i >= 0; i-- {
		out = append(out, m.ops[i])
	}
	return out
}

// Cancel asks a running operation to stop (its context is cancelled).
// found is false for an unknown operation, running false for one that
// already finished (nothing is done).
func (m *Manager) Cancel(id string) (found, running bool) {
	op, ok := m.Get(id)
	if !ok {
		return false, false
	}
	op.mu.Lock()
	running = op.Status == StatusRunning
	op.mu.Unlock()
	if running {
		op.cancel()
	}
	return true, running
}

// Shutdown cancels the running operation and waits for it, or for ctx.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if m.running != nil {
		m.running.cancel()
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
