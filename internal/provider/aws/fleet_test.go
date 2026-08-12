package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/moriyoshi/muster/internal/provider"
)

type fakeECS struct {
	running, desired int32
	empty            bool
	err              error

	gotCluster  string
	gotServices []string
}

func (f *fakeECS) DescribeServices(_ context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	if in.Cluster != nil {
		f.gotCluster = *in.Cluster
	}
	f.gotServices = in.Services
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

func TestFleetAllReplicasRunning(t *testing.T) {
	ctx := context.Background()
	ref := provider.WorkloadRef{Group: "c", Name: "s"}

	fake := &fakeECS{running: 3, desired: 3}
	ok, err := NewFleet(fake).AllReplicasRunning(ctx, ref)
	if err != nil || !ok {
		t.Fatalf("running==desired: ok=%v err=%v", ok, err)
	}
	// The WorkloadRef has to land on the right ECS coordinates; swapping them
	// would still typecheck and would still return a plausible bool.
	if fake.gotCluster != "c" || len(fake.gotServices) != 1 || fake.gotServices[0] != "s" {
		t.Fatalf("cluster=%q services=%v", fake.gotCluster, fake.gotServices)
	}

	if ok, err := NewFleet(&fakeECS{running: 2, desired: 3}).AllReplicasRunning(ctx, ref); err != nil || ok {
		t.Fatalf("running<desired: ok=%v err=%v", ok, err)
	}
	if _, err := NewFleet(&fakeECS{empty: true}).AllReplicasRunning(ctx, ref); err == nil {
		t.Fatal("expected an error when no service matches")
	}
	if _, err := NewFleet(&fakeECS{err: errors.New("boom")}).AllReplicasRunning(ctx, ref); err == nil {
		t.Fatal("expected the DescribeServices error to propagate")
	}
}
