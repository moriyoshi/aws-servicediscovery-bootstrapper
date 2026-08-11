package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

func awaitPromise(t *testing.T, v starlark.Value) (starlark.Value, error) {
	t.Helper()
	p, ok := v.(*promise)
	if !ok {
		t.Fatalf("main did not return a promise, got %s", v.Type())
	}
	return p.await(context.Background())
}

func structField(t *testing.T, v starlark.Value, field string) starlark.Value {
	t.Helper()
	s, ok := v.(*starlarkstruct.Struct)
	if !ok {
		t.Fatalf("not a struct: %s", v.Type())
	}
	f, err := s.Attr(field)
	if err != nil {
		t.Fatalf("attr %s: %v", field, err)
	}
	return f
}

// firstWorkload returns the snapshot of the first registered workload, or false
// if none is registered yet.
func firstWorkload(st *harnessState) (workloadSnapshot, bool) {
	ws := st.snapshot().Workloads
	if len(ws) == 0 {
		return workloadSnapshot{}, false
	}
	return ws[0], true
}

func waitHealth(t *testing.T, st *harnessState, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if w, ok := firstWorkload(st); ok && w.Health.State == want {
			return
		}
		select {
		case <-deadline:
			got := "<none>"
			if w, ok := firstWorkload(st); ok {
				got = w.Health.State
			}
			t.Fatalf("health never became %s (is %s)", want, got)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestMainMustReturnPromise(t *testing.T) {
	eng := testEngine(t, `def main(): return 5`, nil)
	if _, err := eng.callMain(context.Background()); err == nil {
		t.Fatal("expected error: main() must return a promise")
	}
}

func TestSpawnRunsAndExits(t *testing.T) {
	res, err := awaitPromise(t, mustMain(t, `def main(): return spawn(resolve=lambda: ["sh", "-c", "exit 0"])`, &engineDeps{st: newHarnessState()}))
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if code, _ := structField(t, res, "code").(starlark.Int).Int64(); code != 0 {
		t.Fatalf("want code 0, got %v", res)
	}
	if s, _ := starlark.AsString(structField(t, res, "reason")); s != "exited" {
		t.Fatalf("want reason exited, got %v", res)
	}
}

func TestSpawnRespawnExhausted(t *testing.T) {
	_, err := awaitPromise(t, mustMain(t, `
def main():
    return spawn(resolve=lambda: ["sh", "-c", "exit 1"],
                 respawn=True, max_retries=2, initial_interval="1ms", max_interval="1ms")
`, &engineDeps{st: newHarnessState()}))
	if !errors.Is(err, errRetriesExhausted) {
		t.Fatalf("expected errRetriesExhausted, got %v", err)
	}
}

func TestSpawnNoRespawnFailsFast(t *testing.T) {
	_, err := awaitPromise(t, mustMain(t, `def main(): return spawn(resolve=lambda: ["sh", "-c", "exit 3"])`, &engineDeps{st: newHarnessState()}))
	if err == nil {
		t.Fatal("expected a run error without respawn")
	}
}

func TestSpawnReadinessAndSignalStop(t *testing.T) {
	kv := newMemKV("o")
	st := newHarnessState()
	src := `
def readiness(): return poll(lambda: kv_get("up") != None, "5s", interval="10ms")
def main():
    return spawn(resolve=lambda: ["sh", "-c", "sleep 30"], readiness=readiness)
`
	v, err := runMain(t, src, &engineDeps{kv: kv, st: st})
	if err != nil {
		t.Fatalf("main: %v", err)
	}
	p := v.(*promise)

	kv.PutIfAbsent(context.Background(), "up", "1", 0)
	waitHealth(t, st, "healthy", 3*time.Second)

	// graceful stop via signal(); promise resolves with reason "signalled".
	p.doSignal(starlark.None)
	res, err := p.await(context.Background())
	if err != nil {
		t.Fatalf("await after signal: %v", err)
	}
	if s, _ := starlark.AsString(structField(t, res, "reason")); s != "signalled" {
		t.Fatalf("want reason signalled, got %v", res)
	}
}

func TestSpawnLivenessRestart(t *testing.T) {
	kv := newMemKV("o")
	st := newHarnessState()
	// Each attempt clears the "dead" trigger so setting it causes exactly one restart.
	src := `
def resolver():
    kv_delete("dead")
    return ["sh", "-c", "sleep 30"]
def liveness(): return poll(lambda: kv_get("dead") != None, "5s", interval="10ms")
def main():
    return spawn(resolve=resolver, liveness=liveness, respawn=True, restart_on_liveness=True,
                 max_retries=0, initial_interval="1ms", max_interval="1ms", reset_after="1h")
`
	v, err := runMain(t, src, &engineDeps{kv: kv, st: st})
	if err != nil {
		t.Fatalf("main: %v", err)
	}
	p := v.(*promise)

	waitCond(t, func() bool { w, ok := firstWorkload(st); return ok && w.Up }, 2*time.Second, "workload never started")
	kv.PutIfAbsent(context.Background(), "dead", "1", 0)
	waitCond(t, func() bool { w, ok := firstWorkload(st); return ok && w.RespawnCount >= 1 }, 3*time.Second, "no restart on liveness loss")

	p.doSignal(starlark.None)
	_, _ = p.await(context.Background())
}

func TestSpawnPostStartPostStop(t *testing.T) {
	kv := newMemKV("o")
	st := newHarnessState()
	// post_start runs once the process is up; post_stop after it has exited.
	src := `
def post_start(): kv_put_if_absent("started", "1", 0)
def post_stop(): kv_put_if_absent("stopped", "1", 0)
def main():
    return spawn(resolve=lambda: ["sh", "-c", "exit 0"],
                 post_start=post_start, post_stop=post_stop)
`
	if _, err := awaitPromise(t, mustMain(t, src, &engineDeps{kv: kv, st: st})); err != nil {
		t.Fatalf("await: %v", err)
	}
	if v, ok, _ := kv.Get(context.Background(), "started"); !ok || v != "1" {
		t.Fatalf("post_start did not run (got %q, ok=%v)", v, ok)
	}
	if v, ok, _ := kv.Get(context.Background(), "stopped"); !ok || v != "1" {
		t.Fatalf("post_stop did not run (got %q, ok=%v)", v, ok)
	}
}

func TestMultipleWorkloadsRegistered(t *testing.T) {
	kv := newMemKV("o")
	st := newHarnessState()
	// main() coordinates two independent workloads; each has its own readiness
	// signal and its own entry in the snapshot.
	src := `
def ready(k): return lambda: poll(lambda: kv_get(k) != None, "5s", interval="10ms")
def main():
    a = spawn(name="a", resolve=lambda: ["sh", "-c", "sleep 30"], readiness=ready("a"))
    b = spawn(name="b", resolve=lambda: ["sh", "-c", "sleep 30"], readiness=ready("b"))
    return go(lambda: join(a, b))
`
	if _, err := runMain(t, src, &engineDeps{kv: kv, st: st}); err != nil {
		t.Fatalf("main: %v", err)
	}
	kv.PutIfAbsent(context.Background(), "a", "1", 0)
	kv.PutIfAbsent(context.Background(), "b", "1", 0)

	waitCond(t, func() bool {
		ws := st.snapshot().Workloads
		return len(ws) == 2 && ws[0].Health.State == "healthy" && ws[1].Health.State == "healthy"
	}, 3*time.Second, "both workloads never became healthy")

	ws := st.snapshot().Workloads
	if ws[0].Name != "a" || ws[1].Name != "b" {
		t.Fatalf("workload names: got %q, %q; want a, b", ws[0].Name, ws[1].Name)
	}
	if ws[0].PID == 0 || ws[1].PID == 0 || ws[0].PID == ws[1].PID {
		t.Fatalf("expected two distinct running PIDs, got %d and %d", ws[0].PID, ws[1].PID)
	}
}

func TestWorkloadAutoName(t *testing.T) {
	st := newHarnessState()
	if _, err := awaitPromise(t, mustMain(t, `def main(): return spawn(resolve=lambda: ["sh", "-c", "exit 0"])`, &engineDeps{st: st})); err != nil {
		t.Fatalf("await: %v", err)
	}
	ws := st.snapshot().Workloads
	if len(ws) != 1 || ws[0].Name != "workload-0" {
		t.Fatalf("expected one auto-named workload-0, got %+v", ws)
	}
}

func waitCond(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal(msg)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
