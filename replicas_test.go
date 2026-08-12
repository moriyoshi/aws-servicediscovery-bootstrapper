package main

import (
	"context"
	"errors"
	"testing"

	"go.starlark.net/starlark"

	"github.com/moriyoshi/muster/internal/provider"
)

// fakeFleet is the provider-level fake: the builtin only needs a bool and an
// error, so the Starlark tests need no cloud SDK at all.
type fakeFleet struct {
	running bool
	err     error
	// last records the target the builtin resolved, so a test can assert the
	// metadata defaults and the explicit overrides reach the provider.
	last provider.WorkloadRef
}

func (f *fakeFleet) AllReplicasRunning(_ context.Context, ref provider.WorkloadRef) (bool, error) {
	f.last = ref
	if f.err != nil {
		return false, f.err
	}
	return f.running, nil
}

func fleetDeps(f provider.Fleet) *engineDeps {
	return &engineDeps{
		fleet: resolved(f),
		self:  &provider.Identity{Group: "c", Service: "s"},
	}
}

func TestAllReplicasRunning(t *testing.T) {
	src := `def main(): return go(lambda: "yes" if all_replicas_running() else "no")`

	fake := &fakeFleet{running: true}
	v := joinValue(t, mustMain(t, src, fleetDeps(fake)))
	if s, _ := starlark.AsString(v); s != "yes" {
		t.Fatalf("expected stable, got %v", v)
	}
	if fake.last != (provider.WorkloadRef{Group: "c", Name: "s"}) {
		t.Fatalf("target should default to the instance own group/service, got %+v", fake.last)
	}

	v = joinValue(t, mustMain(t, src, fleetDeps(&fakeFleet{running: false})))
	if s, _ := starlark.AsString(v); s != "no" {
		t.Fatalf("expected not-stable, got %v", v)
	}
}

func TestAllReplicasRunningErrors(t *testing.T) {
	join := `def main(): return join(go(lambda: str(all_replicas_running())))`

	// capability not configured
	if _, err := runMain(t, join, &engineDeps{self: &provider.Identity{Group: "c", Service: "s"}}); err == nil {
		t.Fatal("expected error when replica status is not configured")
	}
	// no identity and no explicit group/service
	if _, err := runMain(t, join, &engineDeps{fleet: resolved[provider.Fleet](&fakeFleet{running: true})}); err == nil {
		t.Fatal("expected error when group/service is unknown")
	}
	// API error propagates
	if _, err := runMain(t, join, fleetDeps(&fakeFleet{err: errors.New("boom")})); err == nil {
		t.Fatal("expected the provider error to propagate")
	}
}

func TestWaitForAllReplicasRunning(t *testing.T) {
	v := joinValue(t, mustMain(t, `
def main():
    return go(lambda: "ok" if join(poll(all_replicas_running, "5s")) else "no")
`, fleetDeps(&fakeFleet{running: true})))
	if s, _ := starlark.AsString(v); s != "ok" {
		t.Fatalf("expected ok, got %v", v)
	}
	// explicit group/service override works without identity
	fake := &fakeFleet{running: true}
	v = joinValue(t, mustMain(t, `def main(): return go(lambda: str(all_replicas_running(group="x", service="y")))`,
		&engineDeps{fleet: resolved[provider.Fleet](fake)}))
	if s, _ := starlark.AsString(v); s != "True" {
		t.Fatalf("explicit target: got %v", v)
	}
	if fake.last != (provider.WorkloadRef{Group: "x", Name: "y"}) {
		t.Fatalf("explicit target should reach the provider, got %+v", fake.last)
	}
}
