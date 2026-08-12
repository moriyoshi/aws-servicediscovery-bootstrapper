package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

var promiseIDs atomic.Uint64

// promise is a Go-backed, settle-once future exposed to Starlark. It carries two
// independent, optional capabilities:
//
//   - cancellable: p.cancel() aborts the backing task (or settles a bare promise
//     None). Cancel means "I no longer care about the result."
//   - signallable: p.signal(v) / p.reject(err) let the script settle it from
//     outside. For a bare deferred, signal(v) resolves with v; for spawn(), a
//     signalFn intercepts signal() to request graceful shutdown (the supervisor
//     settles the promise with the real exit result).
//
// A settled promise never changes; the first settle wins. Awaiting is
// ctx-cancellable. Equality is pointer identity (so select() results compare).
type promise struct {
	id          uint64
	name        string
	cancellable bool
	signallable bool
	cancelFn    context.CancelFunc   // task ctx cancel; nil for a bare promise
	signalFn    func(starlark.Value) // custom signal handler (spawn); nil = default settle

	// awaited records that something looked at the outcome -- join(), select(),
	// any_true(). A promise that rejects with this still false is a failure
	// nobody will ever hear about, which spawnFunc reports rather than swallow.
	awaited atomic.Bool

	mu        sync.Mutex
	settled   bool
	canceling bool
	value     starlark.Value
	err       error
	doneCh    chan struct{}
}

func newPromise(name string) *promise {
	return &promise{id: promiseIDs.Add(1), name: name, doneCh: make(chan struct{})}
}

// settle records the outcome exactly once (first wins) and closes doneCh.
func (p *promise) settle(v starlark.Value, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.settled {
		return
	}
	if v == nil {
		v = starlark.None
	}
	p.settled = true
	p.value, p.err = v, err
	close(p.doneCh)
}

func (p *promise) resolve(v starlark.Value) { p.settle(v, nil) }
func (p *promise) reject(err error)         { p.settle(starlark.None, err) }

func (p *promise) isCanceling() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.canceling
}

func (p *promise) isSettled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settled
}

// await blocks until the promise settles or ctx is cancelled. The channel close
// is the happens-before edge, so value/err are read without the lock.
func (p *promise) await(ctx context.Context) (starlark.Value, error) {
	p.awaited.Store(true)
	select {
	case <-p.doneCh:
		return p.value, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// doCancel aborts the task (or settles a bare promise). It sets canceling so a
// resulting context.Canceled from the task settles as resolved(None) rather than
// rejected — an intentional cancel must not raise in join().
func (p *promise) doCancel() {
	p.mu.Lock()
	p.canceling = true
	fn := p.cancelFn
	p.mu.Unlock()
	if fn != nil {
		fn() // task unwinds and settles itself
	} else {
		p.settle(starlark.None, nil) // bare cancellable promise
	}
}

// doSignal delivers an external completion. A signalFn (spawn) intercepts it;
// otherwise the promise resolves with value and any backing task is stopped.
func (p *promise) doSignal(v starlark.Value) {
	if p.signalFn != nil {
		p.signalFn(v)
		return
	}
	p.settle(v, nil)
	if p.cancelFn != nil {
		p.cancelFn()
	}
}

func (p *promise) doReject(err error) {
	if p.signalFn != nil {
		p.signalFn(starlark.None) // spawn: external reject is treated as a stop request
		return
	}
	p.settle(starlark.None, err)
	if p.cancelFn != nil {
		p.cancelFn()
	}
}

// --- starlark.Value ---

func (p *promise) String() string        { return fmt.Sprintf("<promise %s#%d>", p.name, p.id) }
func (p *promise) Type() string          { return "promise" }
func (p *promise) Freeze()               {} // Go-synchronized; safe to "freeze"
func (p *promise) Truth() starlark.Bool  { return starlark.True }
func (p *promise) Hash() (uint32, error) { return uint32(p.id), nil }

func (p *promise) CompareSameType(op syntax.Token, y starlark.Value, _ int) (bool, error) {
	q := y.(*promise)
	switch op {
	case syntax.EQL:
		return p == q, nil
	case syntax.NEQ:
		return p != q, nil
	default:
		return false, fmt.Errorf("promise supports only == and != comparisons")
	}
}

// --- starlark.HasAttrs: done() always; cancel() iff cancellable; signal/reject iff signallable ---

func (p *promise) AttrNames() []string {
	names := []string{"done"}
	if p.cancellable {
		names = append(names, "cancel")
	}
	if p.signallable {
		names = append(names, "signal", "reject")
	}
	return names
}

func (p *promise) Attr(name string) (starlark.Value, error) {
	switch name {
	case "done":
		return starlark.NewBuiltin("done", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if err := starlark.UnpackArgs("done", args, kwargs); err != nil {
				return nil, err
			}
			return starlark.Bool(p.isSettled()), nil
		}), nil
	case "cancel":
		if !p.cancellable {
			return nil, nil
		}
		return starlark.NewBuiltin("cancel", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if err := starlark.UnpackArgs("cancel", args, kwargs); err != nil {
				return nil, err
			}
			p.doCancel()
			return starlark.None, nil
		}), nil
	case "signal":
		if !p.signallable {
			return nil, nil
		}
		return starlark.NewBuiltin("signal", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var v starlark.Value = starlark.None
			if err := starlark.UnpackArgs("signal", args, kwargs, "value?", &v); err != nil {
				return nil, err
			}
			p.doSignal(v)
			return starlark.None, nil
		}), nil
	case "reject":
		if !p.signallable {
			return nil, nil
		}
		return starlark.NewBuiltin("reject", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var msg string
			if err := starlark.UnpackArgs("reject", args, kwargs, "err", &msg); err != nil {
				return nil, err
			}
			p.doReject(errors.New(msg))
			return starlark.None, nil
		}), nil
	}
	return nil, nil
}

var (
	_ starlark.Value      = (*promise)(nil)
	_ starlark.Comparable = (*promise)(nil)
	_ starlark.HasAttrs   = (*promise)(nil)
)
