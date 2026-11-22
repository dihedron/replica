package plugin

import (
	"errors"
	"fmt"
	"testing"
)

func TestClass(t *testing.T) {
	base := errors.New("boom")
	if ClassOf(base) != Unknown || Wrap(nil, Transient) != nil {
		t.Error("an unwrapped error is Unknown; nil stays nil")
	}
	err := fmt.Errorf("context: %w", Wrap(base, Integrity))
	if ClassOf(err) != Integrity || !errors.Is(err, base) {
		t.Errorf("ClassOf through a chain: %v", ClassOf(err))
	}
	if ClassOf(Wrap(Wrap(base, Transient), Permanent)) != Permanent {
		t.Error("the outermost class wins")
	}
	if !errors.Is(fmt.Errorf("x: %w", ErrNotFound), ErrNotFound) || ClassOf(ErrNotFound) != Permanent {
		t.Error("ErrNotFound is Permanent")
	}
}

type fake struct{ Driver }

func (fake) Name() string { return "fake" }

func TestRegistry(t *testing.T) {
	Register(fake{})
	if d, ok := Lookup("fake"); !ok || d.Name() != "fake" {
		t.Error("Lookup after Register")
	}
	defer func() {
		if recover() == nil {
			t.Error("a second Register of a name does not panic")
		}
	}()
	Register(fake{})
}
