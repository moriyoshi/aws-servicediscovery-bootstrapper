package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ecs"

	"github.com/moriyoshi/muster/internal/provider"
)

// Describer is the subset of the ECS client used to check service stability;
// it lets tests inject a fake.
type Describer interface {
	DescribeServices(ctx context.Context, in *ecs.DescribeServicesInput, opts ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
}

// Fleet answers replica-status questions from ECS. WorkloadRef.Group is the ECS
// cluster and WorkloadRef.Name the ECS service; both are resolved to this task's
// own before they get here.
type Fleet struct{ client Describer }

var _ provider.Fleet = (*Fleet)(nil)

func NewFleet(client Describer) *Fleet { return &Fleet{client: client} }

func (f *Fleet) AllReplicasRunning(ctx context.Context, ref provider.WorkloadRef) (bool, error) {
	return serviceStable(ctx, f.client, ref.Group, ref.Name)
}

// serviceStable reports whether the ECS service has RunningCount ==
// DesiredCount (i.e. all its tasks are running).
func serviceStable(ctx context.Context, client Describer, cluster, service string) (bool, error) {
	out, err := client.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  &cluster,
		Services: []string{service},
	})
	if err != nil {
		return false, fmt.Errorf("failed to describe ECS service: %w", err)
	}
	if len(out.Services) == 0 {
		return false, fmt.Errorf("no ECS service found for %s", service)
	}
	return out.Services[0].RunningCount == out.Services[0].DesiredCount, nil
}
