package main

import (
	"fmt"

	"github.com/moriyoshi/muster/internal/provider"
)

// optional is a provider capability that may be absent, carried together with
// the reason it is absent. Startup resolves each capability once; the builtins
// that need one report that reason at the point of use, so a script sees "kv
// store not configured (set -kv-table)" rather than a nil dereference or a
// hardcoded guess at why the field was empty.
type optional[T any] struct {
	val T
	err error
}

// resolved wraps a capability that is available.
func resolved[T any](v T) optional[T] { return optional[T]{val: v} }

// unavailable records why a capability could not be provided. err should read
// as a predicate — "not configured (set -kv-store)" — because require prefixes
// it with the capability's name.
func unavailable[T any](err error) optional[T] { return optional[T]{err: err} }

// capability wraps a Provider accessor's (T, error) return. A capability that
// could not be obtained is not a startup failure: the script may never touch
// it, and if it does the error travels to that call.
func capability[T any](v T, err error) optional[T] {
	if err != nil {
		return unavailable[T](err)
	}
	return resolved(v)
}

func (o optional[T]) get() (T, error) {
	if o.err != nil {
		var zero T
		return zero, o.err
	}
	// The zero value has no error and a nil interface, which would nil-panic on
	// first use. Tests build engineDeps literals and leave the capabilities they
	// do not exercise unset, so treat that as unconfigured rather than a crash.
	if any(o.val) == nil {
		var zero T
		return zero, provider.ErrNotConfigured
	}
	return o.val, nil
}

// require returns the capability, or an error naming it. what is the subject of
// the sentence the reason completes: require("kv store") on an unset field
// yields "kv store not configured".
func (o optional[T]) require(what string) (T, error) {
	v, err := o.get()
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%s %w", what, err)
	}
	return v, nil
}
