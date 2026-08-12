package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"github.com/moriyoshi/muster/internal/provider"
)

const defaultProbeTimeout = 2 * time.Second

// ctxOf retrieves the Go context that hook dispatch stashed on the thread, so
// blocking builtins are cancellable on SIGTERM.
func ctxOf(thread *starlark.Thread) context.Context {
	if c, ok := thread.Local("ctx").(context.Context); ok && c != nil {
		return c
	}
	return context.Background()
}

// lookupIfAddr returns the address of an up, non-loopback, non-point-to-point
// interface that falls within cidr. This is the same selection the old ifaddr
// template function performed.
func lookupIfAddr(cidr string) (string, error) {
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("failed to parse CIDR: %w", err)
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("failed to get interfaces: %w", err)
	}
	for _, if_ := range ifs {
		if if_.Flags&net.FlagUp == 0 || if_.Flags&net.FlagPointToPoint != 0 || if_.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := if_.Addrs()
		if err != nil {
			return "", fmt.Errorf("failed to get interface addresses: %w", err)
		}
		for _, addr := range addrs {
			ip, err := netip.ParsePrefix(addr.String())
			if err != nil {
				return "", fmt.Errorf("failed to parse address: %w", err)
			}
			if pfx.Contains(ip.Addr()) {
				return ip.Addr().String(), nil
			}
		}
	}
	return "", fmt.Errorf("no applicable interfaces found for %s", cidr)
}

func (d *engineDeps) ifAddr(cidr string) (string, error) {
	d.ifMu.Lock()
	defer d.ifMu.Unlock()
	if v, ok := d.ifCache[cidr]; ok {
		return v, nil
	}
	v, err := lookupIfAddr(cidr)
	if err != nil {
		return "", err
	}
	if d.ifCache == nil {
		d.ifCache = map[string]string{}
	}
	d.ifCache[cidr] = v
	return v, nil
}

// waitLoop polls check until it returns true or the timeout elapses, honoring
// ctx cancellation. It returns whether the condition became true. interval is the
// delay between polls; interval<=0 uses a jittered ~500ms default. It backs the
// poll() builtin.
func waitLoop(ctx context.Context, timeout, interval time.Duration, check func(context.Context) (bool, error)) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := check(ctx)
		if err != nil || ok {
			return ok, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		d := interval
		if d <= 0 {
			d = 500*time.Millisecond + time.Duration(rand.Int64N(int64(200*time.Millisecond)))
		}
		if d > remaining {
			d = remaining
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(d):
		}
	}
}

// buildPredeclared constructs the Starlark environment (globals + builtins) the
// script sees. deps carries everything the builtins need at call time.
func buildPredeclared(deps *engineDeps) starlark.StringDict {
	b := func(name string, fn func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error)) *starlark.Builtin {
		return starlark.NewBuiltin(name, fn)
	}

	// PROVIDER is present even when SELF is None, which is exactly when a script
	// needs it: identity failing to resolve is when branching on the platform
	// matters most.
	env := starlark.StringDict{
		"SELF":     selfToStarlark(deps.self),
		"PROVIDER": starlark.String(deps.provider),
		"COMMAND":  stringsToStarlark(deps.command),
	}

	env["instances"] = b("instances", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var service, healthStatus, namespace string
		if err := starlark.UnpackArgs("instances", args, kwargs, "service", &service, "health_status?", &healthStatus, "namespace?", &namespace); err != nil {
			return nil, err
		}
		disc, err := deps.disc.require("service discovery")
		if err != nil {
			return nil, err
		}
		entries, err := disc.Discover(ctxOf(t), provider.Query{
			Namespace: namespace,
			Service:   service,
			Health:    healthStatus,
		})
		if err != nil {
			return nil, err
		}
		return entriesToStarlark(entries), nil
	})

	env["ifaddr"] = b("ifaddr", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var cidr string
		if err := starlark.UnpackArgs("ifaddr", args, kwargs, "cidr", &cidr); err != nil {
			return nil, err
		}
		v, err := deps.ifAddr(cidr)
		if err != nil {
			return nil, err
		}
		return starlark.String(v), nil
	})

	env["path_exists"] = b("path_exists", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var path string
		if err := starlark.UnpackArgs("path_exists", args, kwargs, "path", &path); err != nil {
			return nil, err
		}
		_, err := os.Stat(path)
		return starlark.Bool(err == nil), nil
	})

	env["read_file"] = b("read_file", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var path string
		if err := starlark.UnpackArgs("read_file", args, kwargs, "path", &path); err != nil {
			return nil, err
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		const maxSize = 1 << 20
		data, err := io.ReadAll(io.LimitReader(f, maxSize))
		if err != nil {
			return nil, err
		}
		return starlark.String(string(data)), nil
	})

	// env(name, default=None) reads a variable from the harness's own environment
	// (e.g. a secret ECS injected). Returns default when unset. Distinct from the
	// child's env, which resolve() returns as the "env" dict.
	env["env"] = b("env", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var name string
		var def starlark.Value = starlark.None
		if err := starlark.UnpackArgs("env", args, kwargs, "name", &name, "default?", &def); err != nil {
			return nil, err
		}
		if v, ok := os.LookupEnv(name); ok {
			return starlark.String(v), nil
		}
		return def, nil
	})

	addKVBuiltins(env, b, deps)
	addHealthBuiltins(env, b, deps)
	addReplicaBuiltins(env, b, deps)
	addRegistrationBuiltins(env, b, deps)
	addAsyncBuiltins(env, b, deps)
	addSpawnBuiltin(env, b, deps)

	env["log"] = b("log", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("log: message argument required")
		}
		msg, _ := starlark.AsString(args[0])
		attrs := make([]any, 0, len(kwargs))
		for _, kv := range kwargs {
			key, _ := starlark.AsString(kv[0])
			val, _ := starlark.AsString(kv[1])
			attrs = append(attrs, slog.String(key, val))
		}
		deps.logger.Info(msg, attrs...)
		return starlark.None, nil
	})

	// rand() returns a float in [0.0, 1.0); randint(a, b) returns an int in the
	// inclusive range [a, b], matching Python's random.randint. Handy for
	// jittering a pre_start() delay: sleep(rand() * 5).
	env["rand"] = b("rand", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if err := starlark.UnpackArgs("rand", args, kwargs); err != nil {
			return nil, err
		}
		return starlark.Float(rand.Float64()), nil
	})

	env["randint"] = b("randint", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var lo, hi int
		if err := starlark.UnpackArgs("randint", args, kwargs, "a", &lo, "b", &hi); err != nil {
			return nil, err
		}
		if hi < lo {
			return nil, fmt.Errorf("randint: b (%d) must be >= a (%d)", hi, lo)
		}
		return starlark.MakeInt(lo + rand.IntN(hi-lo+1)), nil
	})

	// un(fn, *args) returns a check factory that computes `not fn(*args)`. It
	// negates a probe factory without a lambda: un(http_ok(url)) for a
	// liveness-lost check, e.g. poll(un(http_ok(url, "2s")), "24h").
	env["un"] = b("un", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(kwargs) > 0 {
			return nil, fmt.Errorf("un: unexpected keyword argument")
		}
		if len(args) < 1 {
			return nil, fmt.Errorf("un: requires a function")
		}
		fn := args[0]
		if _, ok := fn.(starlark.Callable); !ok {
			return nil, fmt.Errorf("un: first argument must be a function, got %s", fn.Type())
		}
		callArgs := args[1:]
		fn.Freeze()
		callArgs.Freeze()
		return b("un", func(t *starlark.Thread, _ *starlark.Builtin, cargs starlark.Tuple, ckwargs []starlark.Tuple) (starlark.Value, error) {
			if err := starlark.UnpackArgs("un", cargs, ckwargs); err != nil {
				return nil, err
			}
			res, err := starlark.Call(t, fn, callArgs, nil)
			if err != nil {
				return nil, err
			}
			return starlark.Bool(!bool(res.Truth())), nil
		}), nil
	})

	env["sleep"] = b("sleep", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var durVal starlark.Value
		if err := starlark.UnpackArgs("sleep", args, kwargs, "seconds", &durVal); err != nil {
			return nil, err
		}
		d, err := unpackDuration(durVal)
		if err != nil {
			return nil, err
		}
		ctx := ctxOf(t)
		select {
		case <-ctx.Done():
			return starlark.None, ctx.Err()
		case <-time.After(d):
		}
		return starlark.None, nil
	})

	if deps.allowRun {
		env["run"] = b("run", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var argvVal starlark.Value
			if err := starlark.UnpackArgs("run", args, kwargs, "argv", &argvVal); err != nil {
				return nil, err
			}
			argv, err := starlarkToStrings(argvVal)
			if err != nil {
				return nil, err
			}
			if len(argv) == 0 {
				return nil, fmt.Errorf("run: argv must be non-empty")
			}
			cmd := exec.CommandContext(ctxOf(t), argv[0], argv[1:]...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			runErr := cmd.Run()
			code := 0
			if runErr != nil {
				code = exitCodeOf(runErr)
			}
			return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
				"code":   starlark.MakeInt(code),
				"stdout": starlark.String(stdout.String()),
				"stderr": starlark.String(stderr.String()),
			}), nil
		})
	}

	return env
}

type builtinFactory = func(string, func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error)) *starlark.Builtin

func addKVBuiltins(env starlark.StringDict, b builtinFactory, deps *engineDeps) {
	requireKV := func() (provider.KVStore, error) {
		return deps.kv.require("kv store")
	}

	env["kv_put_if_absent"] = b("kv_put_if_absent", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var key, val string
		var ttlVal starlark.Value
		if err := starlark.UnpackArgs("kv_put_if_absent", args, kwargs, "key", &key, "val", &val, "ttl?", &ttlVal); err != nil {
			return nil, err
		}
		kv, err := requireKV()
		if err != nil {
			return nil, err
		}
		ttl, err := unpackDuration(ttlVal)
		if err != nil {
			return nil, err
		}
		ok, err := kv.PutIfAbsent(ctxOf(t), key, val, ttl)
		if err != nil {
			return nil, err
		}
		return starlark.Bool(ok), nil
	})

	env["kv_compare_and_swap"] = b("kv_compare_and_swap", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var key, old, new string
		var ttlVal starlark.Value
		if err := starlark.UnpackArgs("kv_compare_and_swap", args, kwargs, "key", &key, "old", &old, "new", &new, "ttl?", &ttlVal); err != nil {
			return nil, err
		}
		kv, err := requireKV()
		if err != nil {
			return nil, err
		}
		ttl, err := unpackDuration(ttlVal)
		if err != nil {
			return nil, err
		}
		ok, err := kv.CompareAndSwap(ctxOf(t), key, old, new, ttl)
		if err != nil {
			return nil, err
		}
		return starlark.Bool(ok), nil
	})

	env["kv_get"] = b("kv_get", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var key string
		if err := starlark.UnpackArgs("kv_get", args, kwargs, "key", &key); err != nil {
			return nil, err
		}
		kv, err := requireKV()
		if err != nil {
			return nil, err
		}
		val, ok, err := kv.Get(ctxOf(t), key)
		if err != nil {
			return nil, err
		}
		if !ok {
			return starlark.None, nil
		}
		return starlark.String(val), nil
	})

	env["kv_delete"] = b("kv_delete", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var key string
		var ifValue starlark.Value
		if err := starlark.UnpackArgs("kv_delete", args, kwargs, "key", &key, "if_value?", &ifValue); err != nil {
			return nil, err
		}
		kv, err := requireKV()
		if err != nil {
			return nil, err
		}
		var cond *string
		if ifValue != nil && ifValue != starlark.None {
			s, ok := starlark.AsString(ifValue)
			if !ok {
				return nil, fmt.Errorf("kv_delete: if_value must be a string")
			}
			cond = &s
		}
		ok, err := kv.Delete(ctxOf(t), key, cond)
		if err != nil {
			return nil, err
		}
		return starlark.Bool(ok), nil
	})

	env["kv_renew"] = b("kv_renew", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var key string
		var ttlVal starlark.Value
		if err := starlark.UnpackArgs("kv_renew", args, kwargs, "key", &key, "ttl", &ttlVal); err != nil {
			return nil, err
		}
		kv, err := requireKV()
		if err != nil {
			return nil, err
		}
		ttl, err := unpackDuration(ttlVal)
		if err != nil {
			return nil, err
		}
		ok, err := kv.Renew(ctxOf(t), key, ttl)
		if err != nil {
			return nil, err
		}
		return starlark.Bool(ok), nil
	})
}

// addReplicaBuiltins registers all_replicas_running(), the orchestrator-level
// precondition. Fold it into pre_start() with poll (e.g.
// join(poll(all_replicas_running, "60s"))). group/service default to the running
// instance's own, from SELF.
//
// The argument order is group-then-service, matching the cluster-then-service it
// replaces: both are optional strings, so reversing them to match instances()
// would let a hand-migrated call swap its two arguments and still typecheck.
func addReplicaBuiltins(env starlark.StringDict, b builtinFactory, deps *engineDeps) {
	resolveTarget := func(groupV, serviceV starlark.Value) (string, string, error) {
		group, service := "", ""
		if deps.self != nil {
			group, service = deps.self.Group, deps.self.Service
		}
		if groupV != nil && groupV != starlark.None {
			s, ok := starlark.AsString(groupV)
			if !ok {
				return "", "", fmt.Errorf("group must be a string")
			}
			group = s
		}
		if serviceV != nil && serviceV != starlark.None {
			s, ok := starlark.AsString(serviceV)
			if !ok {
				return "", "", fmt.Errorf("service must be a string")
			}
			service = s
		}
		if group == "" || service == "" {
			return "", "", fmt.Errorf("group/service unknown (no instance metadata); pass group= and service=")
		}
		return group, service, nil
	}

	env["all_replicas_running"] = b("all_replicas_running", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var groupV, serviceV starlark.Value
		if err := starlark.UnpackArgs("all_replicas_running", args, kwargs, "group?", &groupV, "service?", &serviceV); err != nil {
			return nil, err
		}
		fleet, err := deps.fleet.require("replica status")
		if err != nil {
			return nil, err
		}
		group, service, err := resolveTarget(groupV, serviceV)
		if err != nil {
			return nil, err
		}
		stable, err := fleet.AllReplicasRunning(ctxOf(t), provider.WorkloadRef{Group: group, Name: service})
		if err != nil {
			return nil, err
		}
		return starlark.Bool(stable), nil
	})
}

func addHealthBuiltins(env starlark.StringDict, b builtinFactory, deps *engineDeps) {
	probeArgTimeout := func(v starlark.Value) (time.Duration, error) {
		d, err := unpackDuration(v)
		if err != nil {
			return 0, err
		}
		if d <= 0 {
			d = defaultProbeTimeout
		}
		return d, nil
	}

	// probeFactory wraps a probe as a zero-arg check factory: *_ok(...) captures
	// its target and returns a callable that performs the probe (on the calling
	// task's ctx) and returns a bool. This composes directly with poll/go and
	// un() — e.g. poll(http_ok(url), "60s") — with no lambda. Call it to get
	// the bool inline: http_ok(url)().
	probeFactory := func(name string, probe func(context.Context) bool) *starlark.Builtin {
		return b(name, func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if err := starlark.UnpackArgs(name, args, kwargs); err != nil {
				return nil, err
			}
			return starlark.Bool(probe(ctxOf(t))), nil
		})
	}

	env["http_ok"] = b("http_ok", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var url string
		var timeoutVal starlark.Value
		if err := starlark.UnpackArgs("http_ok", args, kwargs, "url", &url, "timeout?", &timeoutVal); err != nil {
			return nil, err
		}
		timeout, err := probeArgTimeout(timeoutVal)
		if err != nil {
			return nil, err
		}
		return probeFactory("http_ok", func(ctx context.Context) bool { return probeHTTP(ctx, url, timeout) == nil }), nil
	})

	env["tcp_ok"] = b("tcp_ok", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var hostport string
		var timeoutVal starlark.Value
		if err := starlark.UnpackArgs("tcp_ok", args, kwargs, "hostport", &hostport, "timeout?", &timeoutVal); err != nil {
			return nil, err
		}
		timeout, err := probeArgTimeout(timeoutVal)
		if err != nil {
			return nil, err
		}
		return probeFactory("tcp_ok", func(ctx context.Context) bool { return probeTCP(ctx, hostport, timeout) == nil }), nil
	})

	env["grpc_ok"] = b("grpc_ok", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var hostport, service string
		var timeoutVal starlark.Value
		if err := starlark.UnpackArgs("grpc_ok", args, kwargs, "hostport", &hostport, "service?", &service, "timeout?", &timeoutVal); err != nil {
			return nil, err
		}
		timeout, err := probeArgTimeout(timeoutVal)
		if err != nil {
			return nil, err
		}
		return probeFactory("grpc_ok", func(ctx context.Context) bool { return probeGRPCService(ctx, hostport, service, timeout) == nil }), nil
	})

	env["http_request"] = b("http_request", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var method, url string
		var bodyVal, headersVal, timeoutVal starlark.Value
		if err := starlark.UnpackArgs("http_request", args, kwargs, "method", &method, "url", &url, "body?", &bodyVal, "headers?", &headersVal, "timeout?", &timeoutVal); err != nil {
			return nil, err
		}
		timeout, err := unpackDuration(timeoutVal)
		if err != nil {
			return nil, err
		}
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		var body io.Reader
		if bodyVal != nil && bodyVal != starlark.None {
			s, ok := starlark.AsString(bodyVal)
			if !ok {
				return nil, fmt.Errorf("http_request: body must be a string")
			}
			body = bytes.NewReader([]byte(s))
		}
		req, err := http.NewRequestWithContext(ctxOf(t), method, url, body)
		if err != nil {
			return nil, err
		}
		if headersVal != nil && headersVal != starlark.None {
			hd, ok := headersVal.(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("http_request: headers must be a dict")
			}
			for _, item := range hd.Items() {
				k, _ := starlark.AsString(item[0])
				v, _ := starlark.AsString(item[1])
				req.Header.Set(k, v)
			}
		}
		client := &http.Client{Timeout: timeout}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("http_request %s %s: %w", method, url, err)
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, err
		}
		return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
			"status": starlark.MakeInt(resp.StatusCode),
			"body":   starlark.String(string(respBody)),
		}), nil
	})
}

// addRegistrationBuiltins registers register()/deregister(), which publish this
// instance into the provider's service registry so peers can discover it.
//
// They exist because not every platform does it for you. ECS Service Connect
// registers tasks into CloudMap automatically, so the AWS provider reports this
// unsupported and a script calling register() there is told so rather than
// silently doing nothing. Nothing registers a Cloud Run worker pool instance,
// so a clustered workload there has to announce itself -- typically from
// post_start, with deregister() from pre_stop while peers are still reachable.
func addRegistrationBuiltins(env starlark.StringDict, b builtinFactory, deps *engineDeps) {
	env["register"] = b("register", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var service, address, namespace string
		var port int
		if err := starlark.UnpackArgs("register", args, kwargs,
			"service", &service, "port?", &port, "address?", &address, "namespace?", &namespace); err != nil {
			return nil, err
		}
		reg, err := deps.reg.require("registration")
		if err != nil {
			return nil, err
		}
		if err := reg.Register(ctxOf(t), provider.Registration{
			Namespace: namespace,
			Service:   service,
			Address:   address,
			Port:      port,
		}); err != nil {
			return nil, err
		}
		return starlark.None, nil
	})

	env["deregister"] = b("deregister", func(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if err := starlark.UnpackArgs("deregister", args, kwargs); err != nil {
			return nil, err
		}
		reg, err := deps.reg.require("registration")
		if err != nil {
			return nil, err
		}
		if err := reg.Deregister(ctxOf(t)); err != nil {
			return nil, err
		}
		return starlark.None, nil
	})
}
