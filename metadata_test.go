package main

import (
	"testing"

	"go.starlark.net/starlark"

	"github.com/moriyoshi/muster/internal/provider"
)

// The replica-status builtin falls back to the instance's own group/service,
// which is only possible when identity resolved. This is the pairing that broke
// in practice: metadata came back empty, the fallback had nothing to use, and
// the builtin raised on every resolve() attempt, so the workload never started.
//
// Whether the ECS metadata endpoint is read correctly is the AWS provider's
// business and is tested there; this covers the wiring from an Identity to the
// target the builtin resolves.
func TestReplicaTargetFromIdentity(t *testing.T) {
	self := &provider.Identity{Group: "muster-e2e-tikv-main", Service: "tikv-pd"}

	fake := &fakeFleet{running: true}
	v := joinValue(t, mustMain(t,
		`def main(): return go(lambda: "yes" if all_replicas_running() else "no")`,
		&engineDeps{fleet: resolved[provider.Fleet](fake), self: self}))
	if s, _ := starlark.AsString(v); s != "yes" {
		t.Fatalf("all_replicas_running() with an identity-derived target: got %v", v)
	}
	if fake.last != (provider.WorkloadRef{Group: self.Group, Name: self.Service}) {
		t.Fatalf("target should come from the identity, got %+v", fake.last)
	}
}
