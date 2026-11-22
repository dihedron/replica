package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"regexp"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
)

// result is what a step returns to its workflow (§5): DBOS persists it, so
// it carries the class and a sanitised message, never an error value.
type result struct {
	// Err is empty when the step succeeded.
	Err   string       `json:"err,omitempty"`
	Class plugin.Class `json:"class"`
	// Done ends the workflow: the transfer is terminal, or another executor
	// holds it.
	Done bool `json:"done,omitempty"`
	// More: a batch step left parts to copy.
	More bool `json:"more,omitempty"`
}

func (r result) failed() bool { return r.Err != "" }

// driverError marks an error returned by a driver, so that an unclassified
// one stays Unknown (capped, §5) instead of being taken as Transient.
type driverError struct{ err error }

func (e *driverError) Error() string { return e.err.Error() }
func (e *driverError) Unwrap() error { return e.err }

// drv marks err as a driver's; nil stays nil.
func drv(err error) error {
	if err == nil {
		return nil
	}
	return &driverError{err}
}

// fail returns the result of err, classified: the driver's class; for any
// other error (the state store, the secrets, fencing, a context) Transient.
func fail(err error) result {
	class := plugin.ClassOf(err)
	_, fromDriver := errors.AsType[*driverError](err)
	// a driver call cut by fencing or a stop is no fault of the driver's
	stopped := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	if class == plugin.Unknown && (!fromDriver || stopped) {
		class = plugin.Transient
	}
	return result{Err: sanitize(err.Error()), Class: class}
}

var (
	queryString = regexp.MustCompile(`\?[^\s"']*`)
	userInfo    = regexp.MustCompile(`://[^/@\s]*@`)
)

// sanitize removes what may carry a secret from a message (T-06): URL query
// strings (presigned URLs) and user information, and bounds its length.
func sanitize(msg string) string {
	msg = queryString.ReplaceAllString(msg, "?…")
	msg = userInfo.ReplaceAllString(msg, "://…@")
	if len(msg) > 1024 {
		msg = msg[:1024] + "…"
	}
	return msg
}

// backoff is the wait before attempt n+1 (§5): initial × 2^n, at most
// maxInterval, with ±20 % jitter derived from the transfer and n, so that
// it is the same on replay.
func backoff(transfer string, n int, initial, maxInterval time.Duration) time.Duration {
	d := initial
	for range n {
		if d >= maxInterval {
			break
		}
		d *= 2
	}
	d = min(d, maxInterval)
	h := sha256.Sum256(binary.BigEndian.AppendUint64([]byte(transfer), uint64(n))) //nolint:gosec // n is not negative
	f := float64(binary.BigEndian.Uint64(h[:8])) / float64(^uint64(0))             // [0, 1]
	return time.Duration(float64(d) * (0.8 + 0.4*f))
}
