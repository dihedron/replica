package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// AUDL-1
func TestEncode(t *testing.T) {
	var out bytes.Buffer
	l := New("node-a", &Stream{W: &out})
	ctx := WithRequest(context.Background(), Request{Principal: "alice", Groups: []string{"ops"}, RequestID: "r1"})
	l.Emit(ctx, Event{Action: "SECRET_PUT", Outcome: Success, TenantID: "t1", ResourceType: "secret", ResourceName: "s3", Details: map[string]int{"version": 2}})
	l.Emit(context.Background(), Event{Action: "X", Outcome: Failed, Details: strings.Repeat("x", maxDetails)})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines", len(lines))
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"node": "node-a", "seq": 1.0, "action": "SECRET_PUT", "outcome": "success", "tenant_id": "t1",
		"principal": "alice", "request_id": "r1", "resource_name": "s3"} {
		if first[k] != want {
			t.Errorf("%s: %v, want %v", k, first[k], want)
		}
	}
	if fmt.Sprint(first["details"]) != "map[version:2]" || fmt.Sprint(first["groups"]) != "[ops]" {
		t.Errorf("details %v, groups %v", first["details"], first["groups"])
	}
	if _, err := time.Parse(time.RFC3339Nano, first["time"].(string)); err != nil || first["boot"] == "" || first["boot"] != second["boot"] {
		t.Errorf("time %v (%v), boot %v and %v", first["time"], err, first["boot"], second["boot"])
	}
	if second["seq"] != 2.0 || fmt.Sprint(second["details"]) != "map[truncated:true]" {
		t.Errorf("second: %v", second)
	}
	if _, ok := second["tenant_id"]; ok {
		t.Error("an empty field is written")
	}
}

// listen makes a datagram socket standing in for /dev/log.
func listen(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func read(t *testing.T, c *net.UnixConn) string {
	t.Helper()
	buf := make([]byte, 8192)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

// AUDL-2
func TestSyslog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if _, err := DialSyslog(path); err == nil {
		t.Error("no socket: no error")
	}
	server := listen(t, path)
	s, err := DialSyslog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(false, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, server), fmt.Sprintf("<85>veho-audit[%d]: {\"a\":1}\n", os.Getpid()); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if err := s.Write(true, []byte(`{}`)); err != nil || !strings.HasPrefix(read(t, server), "<84>") {
		t.Errorf("warning: %v", err)
	}

	// the socket replaced: the next write reconnects
	_ = server.Close()
	_ = os.Remove(path)
	server = listen(t, path)
	if err := s.Write(false, []byte(`{"b":2}`)); err != nil {
		t.Fatalf("after the socket was replaced: %v", err)
	}
	if got := read(t, server); !strings.HasSuffix(got, "{\"b\":2}\n") {
		t.Errorf("got %q", got)
	}

	// a socket nobody reads: one write times out, then the sink pauses and
	// the next writes fail at once
	start := time.Now()
	for {
		if err = s.Write(false, bytes.Repeat([]byte("x"), 4096)); err != nil {
			break
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("the writes never failed")
		}
	}
	if ne, ok := errors.AsType[net.Error](err); !ok || !ne.Timeout() {
		t.Errorf("a full socket: %v, want a timeout", err)
	}
	start = time.Now()
	for range 100 {
		if err := s.Write(false, []byte(`{}`)); !errors.Is(err, ErrPaused) {
			t.Fatalf("while paused: %v", err)
		}
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("100 writes while paused took %v", d)
	}
}

// AUDL-5: a failed write is logged and counted, and Emit returns.
func TestEmitFailure(t *testing.T) {
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(prev)
	l := New("n", failing{})
	l.Emit(context.Background(), Event{Action: "SECRET_PUT", Outcome: Success})
	if l.Dropped() != 1 || !strings.Contains(logged.String(), "cannot write the audit event") {
		t.Errorf("dropped %d, log %q", l.Dropped(), logged.String())
	}
}

type failing struct{}

func (failing) Write(bool, []byte) error { return errors.New("broken") }

func TestOpen(t *testing.T) {
	if _, err := Open("stderr", "n"); err != nil {
		t.Error(err)
	}
	if _, err := Open("kafka", "n"); err == nil {
		t.Error("an unknown sink: no error")
	}
}
