package plugin

import (
	"errors"
	"fmt"
)

// Class is the class of a driver's error (PLG-4), which decides what the core
// does: retry, fail, restart the upload, re-read, or restart the transfer.
type Class int

const (
	Unknown       Class = iota // not classified: treated with caution by the core
	Transient                  // retry with backoff
	Permanent                  // fail the transfer
	ResumeInvalid              // the upload is gone: begin again
	Integrity                  // the bytes are wrong: rewrite the part
	SourceChanged              // the source changed since it was first read
)

func (c Class) String() string {
	switch c {
	case Transient:
		return "Transient"
	case Permanent:
		return "Permanent"
	case ResumeInvalid:
		return "ResumeInvalid"
	case Integrity:
		return "Integrity"
	case SourceChanged:
		return "SourceChanged"
	default:
		return "Unknown"
	}
}

// ErrNotFound is the error of an absent object, of class Permanent.
var ErrNotFound = Wrap(errors.New("object not found"), Permanent)

// classified is an error with its class.
type classified struct {
	err   error
	class Class
}

func (e *classified) Error() string { return e.err.Error() }
func (e *classified) Unwrap() error { return e.err }

// Wrap gives err a class; nil stays nil.
func Wrap(err error, class Class) error {
	if err == nil {
		return nil
	}
	return &classified{err: err, class: class}
}

// Wrapf is Wrap of fmt.Errorf.
func Wrapf(class Class, format string, args ...any) error {
	return Wrap(fmt.Errorf(format, args...), class)
}

// ClassOf is the class of the outermost classified error in err's chain, or
// Unknown.
func ClassOf(err error) Class {
	if c, ok := errors.AsType[*classified](err); ok {
		return c.class
	}
	return Unknown
}
