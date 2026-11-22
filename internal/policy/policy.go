// Package policy is the distribution policy: the destinations of a job, the
// rule that decides when it is done (M of N copies, spread over the
// topology) and the options of its transfers
// (.specs/veho-distribution-policy.md).
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Values of the options.
const (
	OnSatisfiedComplete = "complete" // keep the other transfers running
	OnSatisfiedStop     = "stop"     // cancel the other transfers
)

// Destination is one copy to create: a registered endpoint and a path on it.
// Its location is the one of the endpoint, never a value the client supplies.
type Destination struct {
	Endpoint string `json:"endpoint"`
	Path     string `json:"path"`
}

// Statement asks for the verified copies to be in at least MinDistinct
// distinct locations of a level.
type Statement struct {
	Level       string `json:"level"`
	MinDistinct int    `json:"min_distinct"`
}

// Completion is the rule that makes a job done: MinCopies of the N
// destinations (0 means all) and the spread statements.
type Completion struct {
	MinCopies int         `json:"min_copies,omitempty"`
	Spread    []Statement `json:"spread,omitempty"`
}

// Duration is a time.Duration that is written as a string ("168h").
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"168h\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Retry bounds the backoff of the retries of a transfer.
type Retry struct {
	Initial     Duration `json:"initial,omitempty"`
	MaxInterval Duration `json:"max_interval,omitempty"`
}

// Policy is the distribution policy of a job.
type Policy struct {
	Destinations []Destination `json:"destinations"`
	Completion   Completion    `json:"completion"`
	OnSatisfied  string        `json:"on_satisfied,omitempty"`
	Verify       string        `json:"verify,omitempty"`
	Overwrite    string        `json:"overwrite,omitempty"`
	Deadline     Duration      `json:"deadline,omitempty"`
	Retry        Retry         `json:"retry,omitempty"`
}

// Parse decodes a policy document strictly (unknown fields and trailing data
// are refused) and applies the defaults. It does not validate: see Validate.
func Parse(data []byte) (Policy, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("policy: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Policy{}, errors.New("policy: unexpected data after the policy document")
	}
	return p.Normalize(), nil
}

// Normalize returns the policy with the defaults applied: all the
// destinations must have a copy, the other transfers run on after the minimum
// is met, SHA-256 verification, fail on a different existing object, a
// deadline of 168 hours and a retry backoff from 2 seconds to 15 minutes.
func (p Policy) Normalize() Policy {
	if len(p.Completion.Spread) == 0 {
		p.Completion.Spread = nil // so that a policy equals itself after a round trip through JSON
	}
	if p.Completion.MinCopies == 0 {
		p.Completion.MinCopies = len(p.Destinations)
	}
	if p.OnSatisfied == "" {
		p.OnSatisfied = OnSatisfiedComplete
	}
	if p.Verify == "" {
		p.Verify = "sha256"
	}
	if p.Overwrite == "" {
		p.Overwrite = "fail"
	}
	if p.Deadline == 0 {
		p.Deadline = Duration(168 * time.Hour)
	}
	if p.Retry.Initial == 0 {
		p.Retry.Initial = Duration(2 * time.Second)
	}
	if p.Retry.MaxInterval == 0 {
		p.Retry.MaxInterval = Duration(15 * time.Minute)
	}
	return p
}
