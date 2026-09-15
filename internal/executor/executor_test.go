package executor

import (
	"context"
	"testing"
)

type namedStub struct{ name string }

func (s namedStub) Name() string { return s.name }
func (s namedStub) Run(context.Context, Task, func(Event)) (*Result, error) {
	return &Result{}, nil
}

func TestRegistryRegisterGetNames(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Get("cline"); ok {
		t.Error("empty registry must not resolve anything")
	}
	r.Register(namedStub{name: "cline"})
	r.Register(namedStub{name: "mock"})

	e, ok := r.Get("cline")
	if !ok || e.Name() != "cline" {
		t.Errorf("Get(cline) = %v, %v", e, ok)
	}
	if _, ok := r.Get("skynet"); ok {
		t.Error("unknown executor must not resolve")
	}
	names := r.Names()
	if len(names) != 2 {
		t.Errorf("Names() = %v, want both registered names", names)
	}
}
