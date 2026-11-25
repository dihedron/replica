package stream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin"
)

// sink is a Send that keeps what it got, or fails after limit bytes.
type sink struct {
	got    bytes.Buffer
	length int64
	limit  int64
	err    error
}

func (s *sink) send(_ context.Context, body io.Reader, length int64) error {
	s.length = length
	if body == nil {
		return nil
	}
	if s.limit > 0 {
		_, _ = io.CopyN(&s.got, body, s.limit)
		return s.err
	}
	_, err := io.Copy(&s.got, body)
	if err != nil {
		return err
	}
	return s.err
}

func TestStream(t *testing.T) {
	ctx := context.Background()
	k := &sink{}
	h := sha256.New()
	s := New("test", 8, h, k.send)
	p0, err := s.Write(ctx, 0, strings.NewReader("abcd"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.Write(ctx, 0, strings.NewReader("abcd"), 4); err != nil || again != p0 {
		t.Fatalf("the same part again: %+v, %v", again, err)
	}
	if _, err := s.Write(ctx, 0, strings.NewReader("xxxx"), 4); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("other bytes: %v", err)
	}
	if _, err := s.Write(ctx, 2, strings.NewReader("efgh"), 4); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("out of order: %v", err)
	}
	if _, err := s.Write(ctx, 1, strings.NewReader("efghi"), 5); plugin.ClassOf(err) != plugin.Permanent {
		t.Fatalf("beyond the size: %v", err)
	}
	p1, err := s.Write(ctx, 1, strings.NewReader("efgh"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if parts := s.Parts(); len(parts) != 2 {
		t.Fatalf("parts %+v", parts)
	}
	if err := s.Close([]plugin.PartResult{p1}); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("wrong parts: %v", err)
	}
	if err := s.Close([]plugin.PartResult{p1, p0}); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("parts out of order: %v", err)
	}
	if err := s.Close([]plugin.PartResult{p0, p1}); err != nil {
		t.Fatal(err)
	}
	if k.got.String() != "abcdefgh" || k.length != 8 {
		t.Fatalf("sent %q as %d", k.got.String(), k.length)
	}
	want := sha256.Sum256([]byte("abcdefgh"))
	if !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatal("the sum did not see every byte")
	}
	if err := s.Close([]plugin.PartResult{p0, p1}); err != nil {
		t.Fatalf("close again: %v", err)
	}
	if _, err := s.Write(ctx, 2, strings.NewReader("x"), 1); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("write after close: %v", err)
	}
}

func TestEmptyAndUnknownSize(t *testing.T) {
	ctx := context.Background()
	k := &sink{}
	s := New("test", 0, nil, k.send)
	p, err := s.Write(ctx, 0, strings.NewReader(""), 0)
	if err != nil || s.Close([]plugin.PartResult{p}) != nil || k.length != 0 {
		t.Fatalf("empty: %v, length %d", err, k.length)
	}
	k = &sink{}
	s = New("test", 0, nil, k.send)
	p, _ = s.Write(ctx, 0, strings.NewReader("xyz"), 3)
	if err := s.Close([]plugin.PartResult{p}); err != nil || k.length != -1 || k.got.String() != "xyz" {
		t.Fatalf("unknown size: %v, length %d, %q", err, k.length, k.got.String())
	}
}

// The request fails: the part's error is its answer, and every later call
// says begin again.
func TestBroken(t *testing.T) {
	ctx := context.Background()
	k := &sink{limit: 2, err: plugin.Wrapf(plugin.Transient, "server gone")}
	s := New("test", 1<<20, nil, k.send)
	if _, err := s.Write(ctx, 0, bytes.NewReader(make([]byte, 1<<20)), 1<<20); plugin.ClassOf(err) != plugin.Transient {
		t.Fatalf("got %v", err)
	}
	if s.Broken() == nil {
		t.Fatal("not broken")
	}
	if _, err := s.Write(ctx, 1, strings.NewReader("x"), 1); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("after: %v", err)
	}
	if err := s.Close(nil); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("close: %v", err)
	}
	// the server ends the request early without an error
	k = &sink{limit: 2}
	s = New("test", 1<<20, nil, k.send)
	if _, err := s.Write(ctx, 0, bytes.NewReader(make([]byte, 1<<20)), 1<<20); plugin.ClassOf(err) != plugin.ResumeInvalid {
		t.Fatalf("ended early: %v", err)
	}
}

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("disk error") }

func TestSourceErrors(t *testing.T) {
	ctx := context.Background()
	s := New("test", 4, nil, (&sink{}).send)
	if _, err := s.Write(ctx, 0, failing{}, 4); plugin.ClassOf(err) != plugin.Transient {
		t.Fatalf("reader error: %v", err)
	}
	s = New("test", 4, nil, (&sink{}).send)
	if _, err := s.Write(ctx, 0, strings.NewReader("ab"), 4); plugin.ClassOf(err) != plugin.Integrity {
		t.Fatalf("short part: %v", err)
	}
	s = New("test", 8, nil, (&sink{}).send)
	p, _ := s.Write(ctx, 0, strings.NewReader("abcd"), 4)
	if err := s.Close([]plugin.PartResult{p}); plugin.ClassOf(err) != plugin.Integrity {
		t.Fatalf("fewer bytes than announced: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	s = New("test", 4, nil, (&sink{}).send)
	if _, err := s.Write(cctx, 0, strings.NewReader("abcd"), 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	s = New("test", 8, nil, (&sink{}).send)
	_, _ = s.Write(ctx, 0, strings.NewReader("abcd"), 4)
	s.Abort()
	if s.Broken() == nil {
		t.Fatal("abort did not stop the stream")
	}
	s.Abort() // again: nothing
}
