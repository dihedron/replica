// Package audit records security-relevant actions as structured events on
// the system log, which the platform forwards to the SIEM
// (.specs/veho-audit.md). The full audit log of veho-audit-housekeeping.md
// (hash chain, signed checkpoints) is in the backlog (item 32).
package audit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// The outcomes of an event.
const (
	Success = "success"
	Denied  = "denied"
	Failed  = "failed"
)

// maxDetails bounds the encoded details (§2).
const maxDetails = 4 << 10

// Event is what the caller says about an action (§2). Details is a
// non-secret value encoded as JSON (INV-5).
type Event struct {
	Action       string
	Outcome      string
	TenantID     string
	Principal    string
	Groups       []string
	ResourceType string
	ResourceID   string
	ResourceName string
	JobID        string
	TransferID   string
	RequestID    string
	WorkflowID   string
	Details      any
}

// Auditor records events. Emit never fails the caller's action.
type Auditor interface {
	Emit(ctx context.Context, e Event)
}

// Request is who acts and in which request; the transport sets it on the
// context, and Emit fills the event's empty fields from it.
type Request struct {
	Principal string
	Groups    []string
	RequestID string
}

type requestKey struct{}

// WithRequest returns ctx carrying r.
func WithRequest(ctx context.Context, r Request) context.Context {
	return context.WithValue(ctx, requestKey{}, r)
}

func fill(ctx context.Context, e Event) Event {
	if r, ok := ctx.Value(requestKey{}).(Request); ok {
		if e.Principal == "" {
			e.Principal, e.Groups = r.Principal, r.Groups
		}
		if e.RequestID == "" {
			e.RequestID = r.RequestID
		}
	}
	return e
}

// record is the encoded event.
type record struct {
	Time         string          `json:"time"`
	Node         string          `json:"node"`
	Boot         string          `json:"boot"`
	Seq          uint64          `json:"seq"`
	Action       string          `json:"action"`
	Outcome      string          `json:"outcome"`
	TenantID     string          `json:"tenant_id,omitempty"`
	Principal    string          `json:"principal,omitempty"`
	Groups       []string        `json:"groups,omitempty"`
	ResourceType string          `json:"resource_type,omitempty"`
	ResourceID   string          `json:"resource_id,omitempty"`
	ResourceName string          `json:"resource_name,omitempty"`
	JobID        string          `json:"job_id,omitempty"`
	TransferID   string          `json:"transfer_id,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	WorkflowID   string          `json:"workflow_id,omitempty"`
	Details      json.RawMessage `json:"details,omitempty"`
}

// Sink writes one encoded event; warn is true for denied and failed ones.
type Sink interface {
	Write(warn bool, event []byte) error
}

// Logger is the Auditor of a process: it numbers the events and writes
// them to its sink.
type Logger struct {
	node, boot string
	sink       Sink
	seq        atomic.Uint64
	dropped    atomic.Uint64
}

// New returns the auditor of node, writing to sink.
func New(node string, sink Sink) *Logger {
	return &Logger{node: node, boot: rand.Text()[:16], sink: sink}
}

// Open returns the auditor of node on the sink named "syslog" (the local
// /dev/log) or "stderr"; syslog fails when the socket is missing.
func Open(sink, node string) (*Logger, error) {
	switch sink {
	case "syslog":
		s, err := DialSyslog(DefaultSocket)
		if err != nil {
			return nil, err
		}
		return New(node, s), nil
	case "stderr":
		return New(node, &Stream{W: os.Stderr}), nil
	}
	return nil, fmt.Errorf("audit: sink %q: want syslog or stderr", sink)
}

// Dropped counts the events that could not be written.
func (l *Logger) Dropped() uint64 { return l.dropped.Load() }

// Emit implements Auditor.
func (l *Logger) Emit(ctx context.Context, e Event) {
	b := l.encode(fill(ctx, e), time.Now())
	if err := l.sink.Write(e.Outcome != Success, b); err != nil {
		l.dropped.Add(1)
		slog.ErrorContext(ctx, "cannot write the audit event", "action", e.Action, "tenant_id", e.TenantID, "error", err)
	}
}

func (l *Logger) encode(e Event, now time.Time) []byte {
	r := record{
		Time: now.UTC().Format(time.RFC3339Nano), Node: l.node, Boot: l.boot, Seq: l.seq.Add(1),
		Action: e.Action, Outcome: e.Outcome, TenantID: e.TenantID, Principal: e.Principal, Groups: e.Groups,
		ResourceType: e.ResourceType, ResourceID: e.ResourceID, ResourceName: e.ResourceName,
		JobID: e.JobID, TransferID: e.TransferID, RequestID: e.RequestID, WorkflowID: e.WorkflowID,
	}
	if e.Details != nil {
		d, err := json.Marshal(e.Details)
		if err != nil || len(d) > maxDetails {
			d = json.RawMessage(`{"truncated":true}`)
		}
		r.Details = d
	}
	b, _ := json.Marshal(r) // a record of strings and raw JSON always encodes
	return b
}

// Stream writes one event per line, for containers and development.
type Stream struct {
	W  io.Writer
	mu sync.Mutex
}

// Write implements Sink.
func (s *Stream) Write(_ bool, event []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.W.Write(append(event, '\n'))
	return err
}

// Recorder keeps the events in memory, for tests.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

// Emit implements Auditor.
func (r *Recorder) Emit(ctx context.Context, e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fill(ctx, e))
}

// Events returns the events emitted so far.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}
