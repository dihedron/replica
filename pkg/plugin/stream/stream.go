// Package stream writes the parts of an upload, in order, into one request,
// for destinations that can neither resume an upload nor take its parts
// apart (Glance, Nexus): the request starts with the first part and ends at
// Close, a part written again with the same bytes is harmless, and once the
// request has failed the upload must begin again.
package stream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/dihedron/veho/pkg/plugin"
)

// Send sends one request with body: of length bytes, -1 when the size was
// not announced (chunked), 0 with a nil body for an empty object. Its error
// is the upload's, classified.
type Send func(ctx context.Context, body io.Reader, length int64) error

// Stream is one attempt's upload.
type Stream struct {
	what string // for messages: "glance: upload of img"
	size int64  // announced at Begin; 0: unknown
	sum  io.Writer
	send Send

	mu      sync.Mutex
	pw      *io.PipeWriter
	cancel  context.CancelFunc
	done    chan error
	written int64
	parts   []plugin.PartResult
	broken  error
	closed  bool
}

// New is a stream of size bytes (0: not announced); sum, when not nil, sees
// every byte sent (the driver's hashes of the object).
func New(what string, size int64, sum io.Writer, send Send) *Stream {
	if sum == nil {
		sum = io.Discard
	}
	return &Stream{what: what, size: size, sum: sum, send: send}
}

// start sends the request in the background, its body the pipe.
func (s *Stream) start(empty bool) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	s.pw, s.cancel, s.done = pw, cancel, make(chan error, 1)
	length := s.size
	var body io.Reader = pr
	switch {
	case empty:
		length, body = 0, nil
	case s.size == 0:
		length = -1
	}
	go func() {
		defer cancel()
		err := s.send(ctx, body, length)
		_ = pr.CloseWithError(err) // unblocks a writer when the request ended early
		s.done <- err
	}()
}

// fail records that the stream broke: the upload must begin again.
func (s *Stream) fail(err error) error {
	if s.broken == nil {
		s.broken = err
		if s.cancel != nil {
			s.cancel()
		}
	}
	return err
}

// Broken is the error that broke the stream, if any.
func (s *Stream) Broken() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broken
}

// Write writes part partNo, the next one, or again one written before.
func (s *Stream) Write(ctx context.Context, partNo int, r io.Reader, length int64) (plugin.PartResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return plugin.PartResult{}, plugin.Wrapf(plugin.ResumeInvalid, "%s: the upload is closed", s.what)
	case s.broken != nil:
		return plugin.PartResult{}, plugin.Wrapf(plugin.ResumeInvalid, "%s broke (%v): begin again", s.what, s.broken)
	case partNo >= 0 && partNo < len(s.parts):
		return s.again(partNo, r, length)
	case partNo != len(s.parts):
		return plugin.PartResult{}, plugin.Wrapf(plugin.ResumeInvalid, "%s: part %d after %d parts: the parts are written in order", s.what, partNo, len(s.parts))
	case length < 0 || s.size > 0 && s.written+length > s.size:
		return plugin.PartResult{}, plugin.Wrapf(plugin.Permanent, "%s: part %d of %d bytes: beyond the %d bytes announced", s.what, partNo, length, s.size)
	}
	if length > 0 && s.pw == nil {
		s.start(false)
	}
	if s.cancel != nil {
		stop := context.AfterFunc(ctx, s.cancel)
		defer stop()
	}
	sha := sha256.New()
	w := io.MultiWriter(sha, s.sum)
	if length > 0 {
		w = io.MultiWriter(s.pw, w)
	}
	src := &reader{r: io.LimitReader(r, length)}
	n, err := io.Copy(w, src)
	s.written += n
	switch {
	case ctx.Err() != nil:
		return plugin.PartResult{}, s.fail(ctx.Err())
	case src.err != nil:
		return plugin.PartResult{}, s.fail(plugin.Wrap(fmt.Errorf("%s: part %d: %w", s.what, partNo, src.err), plugin.Transient))
	case err != nil: // the request ended: its answer says why
		derr := <-s.done
		if derr == nil {
			derr = plugin.Wrapf(plugin.ResumeInvalid, "%s: part %d: the server ended the upload early: begin again", s.what, partNo)
		}
		return plugin.PartResult{}, s.fail(derr)
	case n != length:
		return plugin.PartResult{}, s.fail(plugin.Wrapf(plugin.Integrity, "%s: part %d: %d bytes of %d", s.what, partNo, n, length))
	}
	p := plugin.PartResult{PartNo: partNo, Size: length, SHA256: hex.EncodeToString(sha.Sum(nil))}
	s.parts = append(s.parts, p)
	return p, nil
}

// again is a part written before, written again: harmless with the same
// bytes, which their SHA-256 shows; other bytes cannot replace those sent.
func (s *Stream) again(partNo int, r io.Reader, length int64) (plugin.PartResult, error) {
	p := s.parts[partNo]
	sha := sha256.New()
	n, err := io.Copy(sha, io.LimitReader(r, length))
	if err != nil {
		return plugin.PartResult{}, plugin.Wrap(fmt.Errorf("%s: part %d: %w", s.what, partNo, err), plugin.Transient)
	}
	if n != p.Size || length != p.Size || hex.EncodeToString(sha.Sum(nil)) != p.SHA256 {
		return plugin.PartResult{}, plugin.Wrapf(plugin.ResumeInvalid, "%s: part %d again, with other bytes: begin again", s.what, partNo)
	}
	return p, nil
}

// reader records the error of the part's reader, to tell it from the
// request's.
type reader struct {
	r   io.Reader
	err error
}

func (r *reader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

// Parts are the parts written.
func (s *Stream) Parts() []plugin.PartResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.parts)
}

// Close ends the request once parts are those written and every byte
// announced was sent, and returns its result.
func (s *Stream) Close(parts []plugin.PartResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken != nil {
		return plugin.Wrapf(plugin.ResumeInvalid, "%s broke: begin again", s.what)
	}
	if s.closed {
		return nil
	}
	if len(parts) != len(s.parts) {
		return plugin.Wrapf(plugin.ResumeInvalid, "%s: commit of %d parts, %d written", s.what, len(parts), len(s.parts))
	}
	for i, p := range parts {
		if p.PartNo != i || p.Size != s.parts[i].Size {
			return plugin.Wrapf(plugin.ResumeInvalid, "%s: part %d is not the one written", s.what, i)
		}
	}
	if s.size > 0 && s.written != s.size {
		return s.fail(plugin.Wrapf(plugin.Integrity, "%s: %d bytes written of %d", s.what, s.written, s.size))
	}
	if s.pw == nil { // an empty object
		s.start(true)
	}
	_ = s.pw.Close()
	if err := <-s.done; err != nil {
		return s.fail(err)
	}
	s.closed = true
	return nil
}

// Abort stops the request, if one is running.
func (s *Stream) Abort() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pw != nil && s.broken == nil && !s.closed {
		s.cancel()
		_ = s.pw.CloseWithError(context.Canceled)
		<-s.done
		s.broken = context.Canceled
	}
}
