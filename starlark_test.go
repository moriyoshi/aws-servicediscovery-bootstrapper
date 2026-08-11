package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.starlark.net/starlark"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeScript(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.star")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func testEngine(t *testing.T, src string, deps *engineDeps) *engine {
	t.Helper()
	if deps == nil {
		deps = &engineDeps{}
	}
	if deps.logger == nil {
		deps.logger = testLogger()
	}
	eng, err := loadScript(context.Background(), writeScript(t, src), deps)
	if err != nil {
		t.Fatalf("loadScript: %v", err)
	}
	return eng
}

// runMain loads src and invokes main(), returning its value. Background tasks
// derive from a ctx cancelled on test cleanup, so leaked go()/poll() tasks stop.
func runMain(t *testing.T, src string, deps *engineDeps) (starlark.Value, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eng := testEngine(t, src, deps)
	return eng.invokeValue(ctx, eng.main, nil)
}

// mustMain runs main() and asserts it did not error, returning its value.
func mustMain(t *testing.T, src string, deps *engineDeps) starlark.Value {
	t.Helper()
	v, err := runMain(t, src, deps)
	if err != nil {
		t.Fatalf("main: %v", err)
	}
	return v
}

// joinValue joins a promise and returns the resolved value.
func joinValue(t *testing.T, v starlark.Value) starlark.Value {
	t.Helper()
	p, ok := v.(*promise)
	if !ok {
		t.Fatalf("expected a promise, got %s", v.Type())
	}
	rv, err := p.await(context.Background())
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	return rv
}

func TestMainRequired(t *testing.T) {
	_, err := loadScript(context.Background(), writeScript(t, `X = 1`), &engineDeps{logger: testLogger()})
	if err == nil {
		t.Fatal("expected error when main() is missing")
	}
}

func TestGoJoin(t *testing.T) {
	v := joinValue(t, mustMain(t, `def main(): return go(lambda: 42)`, nil))
	if i, _ := v.(starlark.Int).Int64(); i != 42 {
		t.Fatalf("want 42, got %v", v)
	}
}

func TestJoinRejection(t *testing.T) {
	// join raises when a joined promise rejects.
	_, err := runMain(t, `def main(): return join(go(lambda: fail("boom")))`, nil)
	if err == nil {
		t.Fatal("expected error from rejected promise")
	}
}

func TestJoinMany(t *testing.T) {
	v := joinValue(t, mustMain(t, `
def main():
    return go(lambda: join(go(lambda: 1), go(lambda: 2), go(lambda: 3)))
`, nil))
	list, ok := v.(*starlark.List)
	if !ok || list.Len() != 3 {
		t.Fatalf("want list of 3, got %v", v)
	}
}

func TestSelectIdentity(t *testing.T) {
	// fast promise wins the race; select returns it by identity.
	v := joinValue(t, mustMain(t, `
def main():
    def body():
        fast = go(lambda: "a")
        slow = go(lambda: sleep(60) or "b")
        return "fast" if select(fast, slow) == fast else "slow"
    return go(body)
`, nil))
	if s, _ := starlark.AsString(v); s != "fast" {
		t.Fatalf("want fast, got %v", v)
	}
}

func TestAnyTrue(t *testing.T) {
	// One truthy among falsy → True, short-circuiting past a slow loser.
	v := joinValue(t, mustMain(t, `
def main():
    def body():
        return any_true(go(lambda: False), go(lambda: True), go(lambda: sleep(60) or True))
    return go(body)
`, nil))
	if v != starlark.True {
		t.Fatalf("want True, got %v", v)
	}

	// All falsy → False.
	v = joinValue(t, mustMain(t, `
def main(): return go(lambda: any_true(go(lambda: False), go(lambda: 0), go(lambda: None)))
`, nil))
	if v != starlark.False {
		t.Fatalf("want False, got %v", v)
	}
}

func TestAnyTrueRejects(t *testing.T) {
	_, err := runMain(t, `def main(): return join(go(lambda: any_true(go(lambda: fail("boom")))))`, nil)
	if err == nil {
		t.Fatal("expected any_true to raise on a rejected promise")
	}
}

func TestGoCancel(t *testing.T) {
	start := time.Now()
	v := joinValue(t, mustMain(t, `
def main():
    def body():
        p = go(lambda: sleep(60))
        p.cancel()
        return join(p)
    return go(body)
`, nil))
	if v != starlark.None {
		t.Fatalf("cancelled promise should join to None, got %v", v)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel was not prompt")
	}
}

func TestClosureFreeze(t *testing.T) {
	// mutating captured state after handing the closure to go() is a frozen-value
	// error, not a data race.
	_, err := runMain(t, `
def main():
    box = [0]
    go(lambda: box[0])
    box.append(1)
    return go(lambda: 1)
`, nil)
	if err == nil {
		t.Fatal("expected frozen-value error after go() froze the closure")
	}
}

func TestPromiseSignal(t *testing.T) {
	v := joinValue(t, mustMain(t, `
def main():
    p = promise()
    go(lambda: p.signal("x"))
    return go(lambda: join(p))
`, nil))
	if s, _ := starlark.AsString(v); s != "x" {
		t.Fatalf("want x, got %v", v)
	}
}

func TestPromiseCapabilityErrors(t *testing.T) {
	// cancel() on a non-cancellable promise raises.
	if _, err := runMain(t, `def main():
    p = promise()
    p.cancel()
    return p`, nil); err == nil {
		t.Fatal("expected error calling cancel() on a non-cancellable promise")
	}
}

func TestPoll(t *testing.T) {
	kv := newMemKV("o")
	kv.PutIfAbsent(context.Background(), "up", "1", 0)
	v := joinValue(t, mustMain(t, `
def main():
    return poll(lambda: kv_get("up") != None, "5s", interval="10ms")
`, &engineDeps{kv: kv}))
	if v != starlark.True {
		t.Fatalf("poll should resolve True, got %v", v)
	}

	v2 := joinValue(t, mustMain(t, `def main(): return poll(lambda: False, "200ms", interval="10ms")`, nil))
	if v2 != starlark.False {
		t.Fatalf("poll should time out to False, got %v", v2)
	}
}

func TestEnvBuiltin(t *testing.T) {
	t.Setenv("BOOT_TEST_VAR", "hello")
	v := joinValue(t, mustMain(t, `
def main():
    return go(lambda: env("BOOT_TEST_VAR") + "/" + env("BOOT_MISSING", "fallback"))
`, nil))
	if s, _ := starlark.AsString(v); s != "hello/fallback" {
		t.Fatalf("want hello/fallback, got %v", v)
	}
	// unset with no default yields None.
	v = joinValue(t, mustMain(t, `def main(): return go(lambda: env("BOOT_MISSING") == None)`, nil))
	if v != starlark.True {
		t.Fatalf("missing env should be None, got %v", v)
	}
}

func TestProbeFactory(t *testing.T) {
	// *_ok returns a callable factory; calling it runs the probe. A bad address
	// yields False; un() negates it to True — all lambda-free.
	v := joinValue(t, mustMain(t, `
def main():
    def body():
        down = tcp_ok("127.0.0.1:1", "100ms")   # factory, not yet run
        return [down(), un(down)()]
    return go(body)
`, nil))
	list, ok := v.(*starlark.List)
	if !ok || list.Len() != 2 {
		t.Fatalf("want list of 2, got %v", v)
	}
	if list.Index(0) != starlark.False || list.Index(1) != starlark.True {
		t.Fatalf("want [False, True], got %v", v)
	}
}

func TestProbeComposesWithPoll(t *testing.T) {
	// poll(tcp_ok(...)) times out to False on a dead port; poll(un(...)) resolves
	// True immediately — the factory plugs straight into poll with no lambda.
	v := joinValue(t, mustMain(t, `def main(): return poll(tcp_ok("127.0.0.1:1", "50ms"), "150ms", interval="10ms")`, nil))
	if v != starlark.False {
		t.Fatalf("poll(tcp_ok dead) should be False, got %v", v)
	}
	v = joinValue(t, mustMain(t, `def main(): return poll(un(tcp_ok("127.0.0.1:1", "50ms")), "1s", interval="10ms")`, nil))
	if v != starlark.True {
		t.Fatalf("poll(un(tcp_ok dead)) should be True, got %v", v)
	}
}

func TestRandBuiltins(t *testing.T) {
	v := joinValue(t, mustMain(t, `
def main():
    def body():
        r = rand()
        ok = 0.0 <= r and r < 1.0
        lo, hi = False, False
        for _ in range(500):
            i = randint(2, 5)
            if i < 2 or i > 5: ok = False
            if i == 2: lo = True
            if i == 5: hi = True
        return "ok" if (ok and lo and hi) else "bad"
    return go(body)
`, nil))
	if s, _ := starlark.AsString(v); s != "ok" {
		t.Fatalf("rand builtins out of range: %v", v)
	}
}

func TestKVBuiltins(t *testing.T) {
	kv := newMemKV("o")
	v := joinValue(t, mustMain(t, `
def main():
    kv_put_if_absent("k", "v", 0)
    return go(lambda: kv_get("k"))
`, &engineDeps{kv: kv}))
	if s, _ := starlark.AsString(v); s != "v" {
		t.Fatalf("want v, got %v", v)
	}
}

func TestRunBuiltinGating(t *testing.T) {
	// run() is undefined without -allow-run: name resolution fails at load.
	if _, err := loadScript(context.Background(), writeScript(t, `def main(): return go(lambda: run(["/bin/true"]))`), &engineDeps{logger: testLogger()}); err == nil {
		t.Fatal("expected load error: run() should be undefined without -allow-run")
	}
	// present with allowRun.
	v := joinValue(t, mustMain(t, `def main(): return go(lambda: run(["/bin/echo", "hi"]).code)`, &engineDeps{allowRun: true}))
	if i, _ := v.(starlark.Int).Int64(); i != 0 {
		t.Fatalf("want exit 0, got %v", v)
	}
}

// TestSeedElection is the split-brain safety check: N engines race one lease on a
// shared kv store; exactly one wins "bootstrap".
func TestSeedElection(t *testing.T) {
	const n = 12
	shared := newMemKV("shared")
	src := `
def main():
    role = "bootstrap" if kv_put_if_absent("cluster/seed", "me", 60) else "join"
    return go(lambda: role)
`
	engines := make([]*engine, n)
	for i := range engines {
		engines[i] = testEngine(t, src, &engineDeps{kv: shared})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make([]string, n)
	var wg sync.WaitGroup
	for i, eng := range engines {
		wg.Add(1)
		go func(i int, eng *engine) {
			defer wg.Done()
			v, err := eng.invokeValue(ctx, eng.main, nil)
			if err != nil {
				t.Errorf("main: %v", err)
				return
			}
			rv, err := v.(*promise).await(ctx)
			if err != nil {
				t.Errorf("await: %v", err)
				return
			}
			results[i], _ = starlark.AsString(rv)
		}(i, eng)
	}
	wg.Wait()

	boots := 0
	for _, r := range results {
		if r == "bootstrap" {
			boots++
		}
	}
	if boots != 1 {
		t.Fatalf("expected exactly one bootstrap, got %d (%v)", boots, results)
	}
}
