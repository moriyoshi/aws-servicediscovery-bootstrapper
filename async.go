package main

import (
	"context"
	"fmt"
	"reflect"

	"go.starlark.net/starlark"
)

// markAwaited records that the script has looked at these outcomes, so a
// rejection among them is not reported as one nobody heard about. select() and
// any_true() read doneCh directly rather than through await(), so they have to
// say so themselves.
func markAwaited(ps []*promise) {
	for _, p := range ps {
		p.awaited.Store(true)
	}
}

// addAsyncBuiltins registers the Promise primitives: go(), poll(), promise(),
// join(), select().
func addAsyncBuiltins(env starlark.StringDict, b builtinFactory, deps *engineDeps) {
	// go(fn, *args, signallable=False) -> promise: run fn(*args) asynchronously.
	env["go"] = b("go", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("go: requires a function")
		}
		fn := args[0]
		if _, ok := fn.(starlark.Callable); !ok {
			return nil, fmt.Errorf("go: first argument must be a function, got %s", fn.Type())
		}
		signallable, err := boolKwarg("go", kwargs, "signallable")
		if err != nil {
			return nil, err
		}
		callArgs := args[1:]
		fn.Freeze()
		callArgs.Freeze() // publish captured state as immutable to the new goroutine
		return deps.eng.spawnTask(ctxOf(t), "go", fn, callArgs, signallable), nil
	})

	// poll(check, timeout, interval=1s, signallable=False) -> promise: polls check
	// until truthy (settles True) or timeout (settles False); rejects on a check
	// error. join(poll(...)) is the synchronous wait. The idiomatic readiness/
	// liveness builder.
	env["poll"] = b("poll", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var check, timeoutV, intervalV starlark.Value
		var signallable bool
		if err := starlark.UnpackArgs("poll", args, kwargs, "check", &check, "timeout", &timeoutV, "interval?", &intervalV, "signallable?", &signallable); err != nil {
			return nil, err
		}
		if _, ok := check.(starlark.Callable); !ok {
			return nil, fmt.Errorf("poll: check must be a function, got %s", check.Type())
		}
		timeout, err := unpackDuration(timeoutV)
		if err != nil {
			return nil, err
		}
		if timeout <= 0 {
			return nil, fmt.Errorf("poll: timeout must be positive")
		}
		interval, err := unpackPollInterval(intervalV)
		if err != nil {
			return nil, err
		}
		check.Freeze()
		eng := deps.eng
		p := eng.spawnFunc(ctxOf(t), "poll", func(taskCtx context.Context) (starlark.Value, error) {
			ok, err := waitLoop(taskCtx, timeout, interval, func(c context.Context) (bool, error) {
				v, err := eng.invokeValue(c, check, nil)
				if err != nil {
					return false, err
				}
				return bool(v.Truth()), nil
			})
			if err != nil {
				return nil, err
			}
			return starlark.Bool(ok), nil
		}, signallable)
		return p, nil
	})

	// promise(cancellable=False, signallable=True) -> promise: a bare deferred.
	env["promise"] = b("promise", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		cancellable := false
		signallable := true
		if err := starlark.UnpackArgs("promise", args, kwargs, "cancellable?", &cancellable, "signallable?", &signallable); err != nil {
			return nil, err
		}
		p := newPromise("deferred")
		p.cancellable = cancellable
		p.signallable = signallable
		return p, nil
	})

	// join(*promises) -> value|list: await all; raise on ctx-cancel or rejection.
	env["join"] = b("join", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(kwargs) > 0 {
			return nil, fmt.Errorf("join: unexpected keyword argument")
		}
		ps, err := promiseArgs("join", args)
		if err != nil {
			return nil, err
		}
		ctx := ctxOf(t)
		results := make([]starlark.Value, len(ps))
		for i, p := range ps {
			v, err := p.await(ctx)
			if err != nil {
				return nil, err
			}
			results[i] = v
		}
		if len(results) == 1 {
			return results[0], nil
		}
		return starlark.NewList(results), nil
	})

	// select(*promises) -> promise: race; return the first-settled promise.
	env["select"] = b("select", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(kwargs) > 0 {
			return nil, fmt.Errorf("select: unexpected keyword argument")
		}
		ps, err := promiseArgs("select", args)
		if err != nil {
			return nil, err
		}
		markAwaited(ps)
		ctx := ctxOf(t)
		cases := make([]reflect.SelectCase, 0, len(ps)+1)
		for _, p := range ps {
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(p.doneCh)})
		}
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})
		chosen, _, _ := reflect.Select(cases)
		if chosen == len(ps) {
			return nil, ctx.Err()
		}
		return ps[chosen], nil
	})

	// any_true(*promises) -> bool: race for the first promise that settles with a
	// truthy value. Returns True as soon as one does (cancelling the rest), False
	// once all settle falsy. A rejection raises; ctx-cancel raises. This is the
	// short-circuiting concurrent probe select() can't express (select races on
	// first-settled, not first-truthy).
	env["any_true"] = b("any_true", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(kwargs) > 0 {
			return nil, fmt.Errorf("any_true: unexpected keyword argument")
		}
		ps, err := promiseArgs("any_true", args)
		if err != nil {
			return nil, err
		}
		markAwaited(ps)
		ctx := ctxOf(t)
		cancelRest := func() {
			for _, p := range ps {
				if p.cancellable {
					p.doCancel()
				}
			}
		}
		remaining := append([]*promise(nil), ps...)
		for len(remaining) > 0 {
			cases := make([]reflect.SelectCase, 0, len(remaining)+1)
			for _, p := range remaining {
				cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(p.doneCh)})
			}
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})
			chosen, _, _ := reflect.Select(cases)
			if chosen == len(remaining) {
				return nil, ctx.Err()
			}
			v, perr := remaining[chosen].await(ctx)
			if perr != nil {
				cancelRest()
				return nil, perr
			}
			if v != nil && bool(v.Truth()) {
				cancelRest()
				return starlark.True, nil
			}
			remaining = append(remaining[:chosen], remaining[chosen+1:]...)
		}
		return starlark.False, nil
	})
}

func promiseArgs(name string, args starlark.Tuple) ([]*promise, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("%s: requires at least one promise", name)
	}
	ps := make([]*promise, len(args))
	for i, a := range args {
		p, ok := a.(*promise)
		if !ok {
			return nil, fmt.Errorf("%s: argument %d must be a promise, got %s", name, i+1, a.Type())
		}
		ps[i] = p
	}
	return ps, nil
}

func boolKwarg(fn string, kwargs []starlark.Tuple, want string) (bool, error) {
	val := false
	for _, kv := range kwargs {
		k, _ := starlark.AsString(kv[0])
		if k != want {
			return false, fmt.Errorf("%s: unexpected keyword argument %q", fn, k)
		}
		val = bool(kv[1].Truth())
	}
	return val, nil
}
