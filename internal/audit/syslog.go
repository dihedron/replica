package audit

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// DefaultSocket is the local syslog socket, read by rsyslog, syslog-ng and
// journald.
const DefaultSocket = "/dev/log"

const (
	facilityAuthpriv = 10
	severityWarning  = 4
	severityNotice   = 5
	tag              = "veho-audit"
	writeTimeout     = 250 * time.Millisecond
	// pause after a timeout: a stuck daemon costs one timeout per pause,
	// not one per event
	pause = 5 * time.Second
)

// ErrPaused is a write refused while the sink pauses after a timeout.
var ErrPaused = errors.New("audit: syslog paused after a write timed out")

// Syslog writes events to a local syslog socket (§3): facility authpriv,
// tag veho-audit, the event as the message. Each write has a deadline; a
// write that times out pauses the sink, any other failure reconnects once.
type Syslog struct {
	path   string
	pid    int
	mu     sync.Mutex
	conn   net.Conn
	paused time.Time // writes fail until then
}

// DialSyslog connects to the socket at path, and fails if it cannot.
func DialSyslog(path string) (*Syslog, error) {
	s := &Syslog{path: path, pid: os.Getpid()}
	if err := s.dial(); err != nil {
		return nil, err
	}
	return s, nil
}

// dial tries a datagram socket, then a stream one, as log/syslog does.
func (s *Syslog) dial() error {
	var err error
	for _, network := range []string{"unixgram", "unix"} {
		var c net.Conn
		if c, err = net.Dial(network, s.path); err == nil {
			s.conn = c
			return nil
		}
	}
	return fmt.Errorf("audit: syslog socket %s: %w", s.path, err)
}

// Write implements Sink.
func (s *Syslog) Write(warn bool, event []byte) error {
	sev := severityNotice
	if warn {
		sev = severityWarning
	}
	msg := fmt.Appendf(nil, "<%d>%s[%d]: %s\n", facilityAuthpriv*8+sev, tag, s.pid, event)
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.paused) {
		return ErrPaused
	}
	var err error
	for range 2 {
		if s.conn == nil {
			if err = s.dial(); err != nil {
				continue
			}
		}
		_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err = s.conn.Write(msg); err == nil {
			return nil
		}
		_ = s.conn.Close()
		s.conn = nil
		if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
			s.paused = time.Now().Add(pause)
			break
		}
	}
	return fmt.Errorf("audit: syslog: %w", err)
}

// Close closes the socket.
func (s *Syslog) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}
