package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v5"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// supervisor runs and respawns a workload for one spawn() call, invoking the
// script-supplied callbacks (resolve/pre_start/pre_stop) and readiness/liveness
// promise factories per attempt, and settles the spawn() promise on termination.
type supervisor struct {
	eng       *engine
	logger    *slog.Logger
	w         *workloadState
	cfg       *spawnConfig
	resolveFn starlark.Value
	preStart  starlark.Value // may be nil
	postStart starlark.Value // may be nil
	preStop   starlark.Value // may be nil
	postStop  starlark.Value // may be nil
	readiness starlark.Value // factory, may be nil
	liveness  starlark.Value // factory, may be nil

	p      *promise
	ctx    context.Context
	cancel context.CancelFunc
	stop   bool // set under signalFn: distinguishes w.signal() from harness shutdown
}

// requestStop is the promise's signalFn: a script-initiated graceful stop.
func (sv *supervisor) requestStop() {
	sv.stop = true
	sv.cancel()
}

func (sv *supervisor) run() {
	result, err := sv.loop()
	if err != nil {
		sv.p.reject(err)
		return
	}
	sv.p.resolve(result)
}

func exitStruct(code, respawnCount int, reason string) starlark.Value {
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"code":          starlark.MakeInt(code),
		"respawn_count": starlark.MakeInt(respawnCount),
		"reason":        starlark.String(reason),
	})
}

// callFactory invokes a readiness/liveness factory and returns its promise.
func (sv *supervisor) callFactory(ctx context.Context, fn starlark.Value) (*promise, error) {
	v, err := sv.eng.invokeValue(ctx, fn, nil)
	if err != nil {
		return nil, err
	}
	p, ok := v.(*promise)
	if !ok {
		return nil, fmt.Errorf("readiness/liveness must return a promise, got %s", v.Type())
	}
	return p, nil
}

func (sv *supervisor) loop() (starlark.Value, error) {
	cfg := sv.cfg
	ctx := sv.ctx
	logger := sv.logger
	w := sv.w
	bo := &backoff.ExponentialBackOff{
		InitialInterval:     cfg.respawnInitialInterval,
		RandomizationFactor: backoff.DefaultRandomizationFactor,
		Multiplier:          cfg.respawnMultiplier,
		MaxInterval:         cfg.respawnMaxInterval,
	}
	bo.Reset()
	attempts := 0

	doBackoff := func() error {
		attempts++
		if cfg.respawnMaxRetries != 0 && attempts > cfg.respawnMaxRetries {
			return errRetriesExhausted
		}
		d := bo.NextBackOff()
		if d == backoff.Stop {
			return errRetriesExhausted
		}
		w.incRespawn(d)
		logger.Info("respawning workload", slog.Duration("backoff", d), slog.Int("attempt", attempts), slog.Int("max_retries", cfg.respawnMaxRetries))
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
		return nil
	}

	for {
		if ctx.Err() != nil {
			return exitStruct(0, attempts, sv.stopReason()), nil
		}

		// Re-resolve the workload command on every (re)spawn.
		resolveCtx := ctx
		var resolveCancel context.CancelFunc
		if cfg.resolveTimeout > 0 {
			resolveCtx, resolveCancel = context.WithTimeout(ctx, cfg.resolveTimeout)
		}
		rv, rerr := sv.eng.invokeValue(resolveCtx, sv.resolveFn, nil)
		var argv, extraEnv []string
		if rerr == nil {
			argv, extraEnv, rerr = parseResolveResult(rv)
		}
		if resolveCancel != nil {
			resolveCancel()
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return exitStruct(0, attempts, sv.stopReason()), nil
			}
			logger.Error("resolve failed", slog.String("err", rerr.Error()))
			if !cfg.respawnEnabled || strings.EqualFold(cfg.resolveFailure, "fail") {
				return nil, fmt.Errorf("resolve failed: %w", rerr)
			}
			if berr := doBackoff(); berr != nil {
				return nil, berr
			}
			continue
		}

		bin, lerr := exec.LookPath(argv[0])
		if lerr != nil {
			return nil, fmt.Errorf("command not found: %s", argv[0])
		}
		argv[0] = bin

		if sv.preStart != nil {
			if _, err := sv.eng.invokeValue(ctx, sv.preStart, nil); err != nil {
				if ctx.Err() != nil {
					return exitStruct(0, attempts, sv.stopReason()), nil
				}
				return nil, fmt.Errorf("pre_start failed: %w", err)
			}
		}

		logger.Info("running", slog.Any("argv", argv))
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), extraEnv...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin

		startedAt := time.Now()
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("failed to start command: %w", err)
		}
		pid := cmd.Process.Pid
		w.setUp(pid)
		logger.Info("workload started", slog.Int("pid", pid), slog.Int("attempt", attempts))

		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }()

		// Per-attempt ctx for the readiness/liveness watchers; cancelled on teardown.
		attemptCtx, attemptCancel := context.WithCancel(ctx)
		readinessFailed := make(chan struct{}, 1)
		livenessLost := make(chan struct{}, 1)
		if sv.readiness != nil {
			go sv.watchReadiness(attemptCtx, readinessFailed)
		}
		if sv.liveness != nil {
			go sv.watchLiveness(attemptCtx, livenessLost)
		}

		// post_start runs once the process is up (PID known). Best-effort: a
		// failure is logged, not fatal — use readiness to gate on being ready.
		if sv.postStart != nil {
			logger.Info("running post_start")
			if _, err := sv.eng.invokeValue(ctx, sv.postStart, nil); err != nil && ctx.Err() == nil {
				logger.Warn("post_start failed", slog.String("err", err.Error()))
			}
		}

		terminate := func() error {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case werr := <-waitCh:
				return werr
			case <-time.After(cfg.shutdownGrace):
				_ = cmd.Process.Kill()
				return <-waitCh
			}
		}
		// runStopHook invokes a pre_stop/post_stop callback (best-effort). When
		// detached it runs under a fresh, time-bounded ctx (pre_stop_timeout, else
		// shutdown_grace) so teardown work still runs after the harness ctx is done.
		runStopHook := func(name string, fn starlark.Value, detached bool) {
			if fn == nil {
				return
			}
			hookCtx := ctx
			var cancel context.CancelFunc
			if detached {
				budget := cfg.preStopTimeout
				if budget <= 0 {
					budget = cfg.shutdownGrace
				}
				hookCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), budget)
			}
			logger.Info("running " + name)
			if _, err := sv.eng.invokeValue(hookCtx, fn, nil); err != nil {
				logger.Warn(name+" failed", slog.String("err", err.Error()))
			}
			if cancel != nil {
				cancel()
			}
		}
		runPreStop := func(detached bool) { runStopHook("pre_stop", sv.preStop, detached) }

		var waitErr error
		healthTriggered := false
		select {
		case <-ctx.Done():
			attemptCancel()
			reason := sv.stopReason()
			runPreStop(true)
			waitErr = terminate()
			w.setDown(exitCodeOf(waitErr), waitErr)
			logger.Info("workload torn down", slog.String("reason", reason), slog.String("err", errString(waitErr)))
			runStopHook("post_stop", sv.postStop, true)
			return exitStruct(exitCodeOf(waitErr), attempts, reason), nil
		case <-readinessFailed:
			attemptCancel()
			healthTriggered = true
			logger.Warn("workload failed readiness; restarting", slog.Int("pid", pid))
			runPreStop(false)
			waitErr = terminate()
		case <-livenessLost:
			attemptCancel()
			healthTriggered = true
			logger.Warn("workload lost liveness; restarting", slog.Int("pid", pid))
			runPreStop(false)
			waitErr = terminate()
		case waitErr = <-waitCh:
			attemptCancel()
		}

		code := exitCodeOf(waitErr)
		w.setDown(code, waitErr)
		uptime := time.Since(startedAt)
		logger.Info("workload exited", slog.Int("exit_code", code), slog.String("err", errString(waitErr)), slog.Duration("uptime", uptime))
		runStopHook("post_stop", sv.postStop, ctx.Err() != nil)

		if ctx.Err() != nil {
			return exitStruct(code, attempts, sv.stopReason()), nil
		}

		if uptime >= cfg.respawnResetAfter {
			bo.Reset()
			attempts = 0
			w.resetRespawn()
		}

		if !healthTriggered {
			if !cfg.respawnEnabled {
				if waitErr != nil {
					return nil, fmt.Errorf("failed to run command: %w", waitErr)
				}
				return exitStruct(code, attempts, "exited"), nil
			}
			if code == 0 && !cfg.respawnKeepAlive {
				return exitStruct(0, attempts, "exited"), nil
			}
		}

		if berr := doBackoff(); berr != nil {
			return nil, berr
		}
	}
}

func (sv *supervisor) stopReason() string {
	if sv.stop {
		return "signalled"
	}
	return "shutdown"
}

// watchReadiness awaits the readiness factory's promise; a truthy resolve marks
// the workload healthy, a reject/false requests a restart.
func (sv *supervisor) watchReadiness(ctx context.Context, failed chan<- struct{}) {
	rp, err := sv.callFactory(ctx, sv.readiness)
	if err != nil {
		if ctx.Err() == nil {
			sv.logger.Warn("readiness factory failed", slog.String("err", err.Error()))
			select {
			case failed <- struct{}{}:
			default:
			}
		}
		return
	}
	v, err := rp.await(ctx)
	if err != nil {
		return // attempt teardown
	}
	if v.Truth() {
		sv.w.setHealth(healthHealthy, 1, 0, nil)
		sv.logger.Info("workload became ready")
		return
	}
	select {
	case failed <- struct{}{}:
	default:
	}
}

// watchLiveness awaits the liveness factory's promise; a truthy settle means
// liveness was lost — mark unhealthy and, if configured, request a restart.
func (sv *supervisor) watchLiveness(ctx context.Context, lost chan<- struct{}) {
	lp, err := sv.callFactory(ctx, sv.liveness)
	if err != nil {
		if ctx.Err() == nil {
			sv.logger.Warn("liveness factory failed", slog.String("err", err.Error()))
		}
		return
	}
	v, err := lp.await(ctx)
	if err != nil {
		return // attempt teardown
	}
	if !v.Truth() {
		return
	}
	sv.w.setHealth(healthUnhealthy, 0, 1, nil)
	sv.logger.Warn("workload lost liveness")
	if sv.cfg.restartOnLiveness {
		select {
		case lost <- struct{}{}:
		default:
		}
	}
}

// addSpawnBuiltin registers spawn().
func addSpawnBuiltin(env starlark.StringDict, b builtinFactory, deps *engineDeps) {
	env["spawn"] = b("spawn", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		cfg := defaultSpawnConfig()
		var resolveFn, preStart, postStart, preStop, postStop, readiness, liveness starlark.Value
		var name string
		respawn := cfg.respawnEnabled
		keepAlive := cfg.respawnKeepAlive
		maxRetries := cfg.respawnMaxRetries
		multiplier := cfg.respawnMultiplier
		resolveFailure := cfg.resolveFailure
		restartOnLiveness := cfg.restartOnLiveness
		var initialV, maxV, resetV, graceV, preStopTV, resolveTV starlark.Value

		if err := starlark.UnpackArgs("spawn", args, kwargs,
			"resolve", &resolveFn,
			"name?", &name,
			"pre_start?", &preStart,
			"post_start?", &postStart,
			"pre_stop?", &preStop,
			"post_stop?", &postStop,
			"readiness?", &readiness,
			"liveness?", &liveness,
			"respawn?", &respawn,
			"keep_alive?", &keepAlive,
			"max_retries?", &maxRetries,
			"initial_interval?", &initialV,
			"max_interval?", &maxV,
			"multiplier?", &multiplier,
			"reset_after?", &resetV,
			"shutdown_grace?", &graceV,
			"pre_stop_timeout?", &preStopTV,
			"resolve_timeout?", &resolveTV,
			"resolve_failure?", &resolveFailure,
			"restart_on_liveness?", &restartOnLiveness,
		); err != nil {
			return nil, err
		}

		for name, fn := range map[string]starlark.Value{
			"resolve": resolveFn, "pre_start": preStart, "post_start": postStart,
			"pre_stop": preStop, "post_stop": postStop,
			"readiness": readiness, "liveness": liveness,
		} {
			if fn == nil {
				continue
			}
			if _, ok := fn.(starlark.Callable); !ok {
				return nil, fmt.Errorf("spawn: %s must be a function, got %s", name, fn.Type())
			}
			fn.Freeze()
		}
		if resolveFn == nil {
			return nil, fmt.Errorf("spawn: resolve is required")
		}

		cfg.respawnEnabled = respawn
		cfg.respawnKeepAlive = keepAlive
		cfg.respawnMaxRetries = maxRetries
		cfg.respawnMultiplier = multiplier
		cfg.resolveFailure = resolveFailure
		cfg.restartOnLiveness = restartOnLiveness
		for _, d := range []struct {
			v starlark.Value
			t *time.Duration
		}{
			{initialV, &cfg.respawnInitialInterval}, {maxV, &cfg.respawnMaxInterval},
			{resetV, &cfg.respawnResetAfter}, {graceV, &cfg.shutdownGrace},
			{preStopTV, &cfg.preStopTimeout}, {resolveTV, &cfg.resolveTimeout},
		} {
			if d.v != nil && d.v != starlark.None {
				dur, err := unpackDuration(d.v)
				if err != nil {
					return nil, err
				}
				*d.t = dur
			}
		}
		if cfg.respawnEnabled {
			if cfg.respawnMultiplier <= 1 {
				return nil, fmt.Errorf("spawn: multiplier must be greater than 1")
			}
			if cfg.respawnMaxRetries < 0 {
				return nil, fmt.Errorf("spawn: max_retries must be >= 0")
			}
		}
		switch strings.ToLower(cfg.resolveFailure) {
		case "retry", "fail":
		default:
			return nil, fmt.Errorf("spawn: invalid resolve_failure %q", cfg.resolveFailure)
		}

		st := deps.st
		if st == nil {
			st = newHarnessState()
		}
		w := st.register(name, cfg.respawnMaxRetries)
		svctx, cancel := context.WithCancel(ctxOf(t))
		p := newPromise(w.name)
		p.signallable = true
		sv := &supervisor{
			eng: deps.eng, logger: deps.logger.With(slog.String("workload", w.name)), w: w, cfg: cfg,
			resolveFn: resolveFn, preStart: preStart, postStart: postStart,
			preStop: preStop, postStop: postStop,
			readiness: readiness, liveness: liveness,
			p: p, ctx: svctx, cancel: cancel,
		}
		p.signalFn = func(_ starlark.Value) { sv.requestStop() }
		go sv.run()
		return p, nil
	})
}
