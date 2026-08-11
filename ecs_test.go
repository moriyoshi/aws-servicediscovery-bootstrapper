package main

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"go.starlark.net/starlark"
)

type fakeECS struct {
	running, desired int32
	empty            bool
	err              error
}

func (f *fakeECS) DescribeServices(_ context.Context, _ *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.empty {
		return &ecs.DescribeServicesOutput{}, nil
	}
	return &ecs.DescribeServicesOutput{
		Services: []ecstypes.Service{{RunningCount: f.running, DesiredCount: f.desired}},
	}, nil
}

func ecsDeps(fake ecsDescriber) *engineDeps {
	return &engineDeps{ecs: fake, meta: &taskMetadataV4{Cluster: "c", ServiceName: "s"}}
}

func TestAllEcsTasksRunning(t *testing.T) {
	src := `def main(): return go(lambda: "yes" if all_ecs_tasks_running() else "no")`

	v := joinValue(t, mustMain(t, src, ecsDeps(&fakeECS{running: 3, desired: 3})))
	if s, _ := starlark.AsString(v); s != "yes" {
		t.Fatalf("expected stable, got %v", v)
	}
	v = joinValue(t, mustMain(t, src, ecsDeps(&fakeECS{running: 2, desired: 3})))
	if s, _ := starlark.AsString(v); s != "no" {
		t.Fatalf("expected not-stable, got %v", v)
	}
}

func TestAllEcsTasksRunningErrors(t *testing.T) {
	join := `def main(): return join(go(lambda: str(all_ecs_tasks_running())))`

	// ECS not configured
	if _, err := runMain(t, join, &engineDeps{meta: &taskMetadataV4{Cluster: "c", ServiceName: "s"}}); err == nil {
		t.Fatal("expected error when ECS client is not configured")
	}
	// no metadata and no explicit cluster/service
	if _, err := runMain(t, join, &engineDeps{ecs: &fakeECS{running: 1, desired: 1}}); err == nil {
		t.Fatal("expected error when cluster/service is unknown")
	}
	// API error propagates
	if _, err := runMain(t, join, ecsDeps(&fakeECS{err: errors.New("boom")})); err == nil {
		t.Fatal("expected the DescribeServices error to propagate")
	}
}

func TestWaitForEcsStable(t *testing.T) {
	v := joinValue(t, mustMain(t, `
def main():
    return go(lambda: "ok" if join(poll(all_ecs_tasks_running, "5s")) else "no")
`, ecsDeps(&fakeECS{running: 1, desired: 1})))
	if s, _ := starlark.AsString(v); s != "ok" {
		t.Fatalf("expected ok, got %v", v)
	}
	// explicit cluster/service override works without metadata
	v = joinValue(t, mustMain(t, `def main(): return go(lambda: str(all_ecs_tasks_running(cluster="x", service="y")))`, &engineDeps{ecs: &fakeECS{running: 2, desired: 2}}))
	if s, _ := starlark.AsString(v); s != "True" {
		t.Fatalf("explicit target: got %v", v)
	}
}
