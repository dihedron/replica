package secrets

import (
	"fmt"
	"io"
	"log/slog"
)

const redacted = "[redacted]"

// Secret is a plaintext value held for one attempt (SEC-10): it prints as
// [redacted] however it is printed, logged or marshalled, and Zero clears it.
type Secret struct{ b []byte }

// Bytes is the value; it is valid until Zero.
func (s *Secret) Bytes() []byte { return s.b }

// Zero clears the value; the holder defers it.
func (s *Secret) Zero() { clear(s.b) }

func (s *Secret) String() string               { return redacted }
func (s *Secret) GoString() string             { return redacted }
func (s *Secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }
func (s *Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (s *Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (s *Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
