package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.starlark.net/starlark"

	"github.com/moriyoshi/muster/internal/provider"
)

// engineDeps carries everything the Starlark builtins need at call time. It is
// shared across concurrent hook/task invocations; the only mutable field is the
// ifMu-guarded ifCache. Module globals are frozen after load, so concurrent
// starlark.Call across threads is safe.
type engineDeps struct {
	logger *slog.Logger

	// Provider capabilities, each carrying the reason it is unavailable so the
	// builtin that needs one can say why (see optional).
	disc  optional[provider.Discoverer]
	kv    optional[provider.KVStore]
	fleet optional[provider.Fleet]
	reg   optional[provider.Registrar]

	// self is the identity of the instance muster runs on, or nil when the
	// platform did not tell us. It backs the SELF global and defaults the
	// target of all_replicas_running().
	self *provider.Identity

	// provider is the selected provider's name, exposed as PROVIDER. It is set
	// even when self is nil.
	provider string

	command  []string
	allowRun bool

	eng *engine       // back-reference so go()/spawn() can launch tasks
	st  *harnessState // shared control-socket state, set by doIt

	ifMu    sync.Mutex
	ifCache map[string]string
}

// engine wraps a loaded Starlark script. main() is the only required global; all
// other functions are ordinary values the script wires into spawn() by reference.
type engine struct {
	deps    *engineDeps
	globals starlark.StringDict
	main    *starlark.Function

	// unobservedGrace is how long a rejected task is given for someone to join
	// it before the failure is reported as unobserved. Overridden in tests.
	unobservedGrace time.Duration
}

// defaultUnobservedGrace is long enough that a task rejecting just before its
// joiner reaches join() is not reported, and short enough that a genuinely
// dropped failure appears while it still explains what follows.
const defaultUnobservedGrace = 5 * time.Second

// loadScript compiles and evaluates the script at path and extracts main().
func loadScript(ctx context.Context, path string, deps *engineDeps) (*engine, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read script: %w", err)
	}
	e := &engine{deps: deps}
	deps.eng = e
	predeclared := buildPredeclared(deps)
	thread := &starlark.Thread{Name: "load", Print: printFunc(deps.logger)}
	thread.SetLocal("ctx", ctx)
	globals, err := starlark.ExecFile(thread, path, src, predeclared)
	if err != nil {
		return nil, fmt.Errorf("failed to load script %s: %w", path, err)
	}
	e.globals = globals
	mainFn, ok := globals["main"].(*starlark.Function)
	if !ok {
		return nil, fmt.Errorf("script must define a main() function")
	}
	e.main = mainFn
	return e, nil
}

func printFunc(logger *slog.Logger) func(*starlark.Thread, string) {
	return func(_ *starlark.Thread, msg string) {
		logger.Info("starlark", slog.String("print", msg))
	}
}

// invokeValue runs a callable on a fresh, ctx-carrying thread. A watchdog cancels
// the interpreter if ctx is done, so a callable stuck in a blocking builtin or a
// pure-Starlark loop is interruptible. There is no global lock: module globals
// are frozen, so concurrent invocations across goroutines are safe.
func (e *engine) invokeValue(ctx context.Context, fn starlark.Value, args starlark.Tuple) (starlark.Value, error) {
	name := "task"
	if n, ok := fn.(interface{ Name() string }); ok {
		name = n.Name()
	}
	thread := &starlark.Thread{Name: name, Print: printFunc(e.deps.logger)}
	thread.SetLocal("ctx", ctx)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel("context canceled")
		case <-done:
		}
	}()
	defer close(done)
	return starlark.Call(thread, fn, args, nil)
}

// spawnFunc runs a Go closure asynchronously on its own cancellable task ctx and
// returns a cancellable promise settled with the closure's result. An intentional
// cancel settles as resolved(None) rather than rejected.
func (e *engine) spawnFunc(ctx context.Context, name string, run func(context.Context) (starlark.Value, error), signallable bool) *promise {
	taskCtx, cancel := context.WithCancel(ctx)
	p := newPromise(name)
	p.cancellable = true
	p.cancelFn = cancel
	p.signallable = signallable
	go func() {
		defer cancel()
		v, err := run(taskCtx)
		if err != nil {
			if p.isCanceling() {
				p.settle(starlark.None, nil)
				return
			}
			p.reject(err)
			e.reportIfUnobserved(p, err)
			return
		}
		p.resolve(v)
	}()
	return p
}

// reportIfUnobserved logs a task failure that nothing is going to join.
//
// A rejected promise is inert: it holds the error until someone asks, and if
// nobody ever does, the error is simply gone. That is right for a probe raced
// by any_true() and wrong for a background task, and the difference is invisible
// from here -- so this waits a moment for a joiner and reports what is left.
//
// This exists because of a real failure. A script started a reporting loop with
// go(report) and never joined it; the first HTTP request in the loop ran before
// the workload was listening, http_request raised on the refused connection, and
// the task died on its first iteration. Nothing was logged, the workload was
// healthy, and the loop's total silence was indistinguishable from a loop that
// had nothing to say. It cost a full cloud provision to find.
//
// A joined promise reports through its joiner, so staying quiet in that case is
// what keeps this from double-reporting every ordinary error.
func (e *engine) reportIfUnobserved(p *promise, err error) {
	grace := e.unobservedGrace
	if grace <= 0 {
		grace = defaultUnobservedGrace
	}
	go func() {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		<-timer.C
		if p.awaited.Load() {
			return
		}
		e.deps.logger.Warn("task failed and nothing joined it; its error would "+
			"otherwise be lost", "task", p.name, "err", err)
	}()
}

// spawnTask is spawnFunc for a Starlark callable (the go() builtin).
func (e *engine) spawnTask(ctx context.Context, name string, fn starlark.Value, args starlark.Tuple, signallable bool) *promise {
	return e.spawnFunc(ctx, name, func(taskCtx context.Context) (starlark.Value, error) {
		return e.invokeValue(taskCtx, fn, args)
	}, signallable)
}

// callMain runs main() and requires it to return a promise.
func (e *engine) callMain(ctx context.Context) (*promise, error) {
	v, err := e.invokeValue(ctx, e.main, nil)
	if err != nil {
		return nil, err
	}
	p, ok := v.(*promise)
	if !ok {
		return nil, fmt.Errorf("main() must return a promise, got %s", v.Type())
	}
	return p, nil
}

func parseResolveResult(v starlark.Value) ([]string, []string, error) {
	switch t := v.(type) {
	case *starlark.Dict:
		argvVal, found, err := t.Get(starlark.String("argv"))
		if err != nil {
			return nil, nil, err
		}
		if !found {
			return nil, nil, fmt.Errorf("resolve(): result dict must contain 'argv'")
		}
		argv, err := starlarkToStrings(argvVal)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve(): argv: %w", err)
		}
		var env []string
		if envVal, found, _ := t.Get(starlark.String("env")); found && envVal != starlark.None {
			env, err = starlarkDictToEnv(envVal)
			if err != nil {
				return nil, nil, fmt.Errorf("resolve(): env: %w", err)
			}
		}
		if len(argv) == 0 {
			return nil, nil, fmt.Errorf("resolve(): argv must be non-empty")
		}
		return argv, env, nil
	case *starlark.List, starlark.Tuple:
		argv, err := starlarkToStrings(v)
		if err != nil {
			return nil, nil, err
		}
		if len(argv) == 0 {
			return nil, nil, fmt.Errorf("resolve(): argv must be non-empty")
		}
		return argv, nil, nil
	default:
		return nil, nil, fmt.Errorf("resolve() must return a dict{argv,env} or a list of argv strings, got %s", v.Type())
	}
}
