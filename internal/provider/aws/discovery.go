// Package aws implements muster's provider capabilities on AWS: CloudMap for
// discovery, the ECS task metadata endpoint for identity, ECS DescribeServices
// for replica status, and DynamoDB for the conditional-write kv store.
//
// It is compiled unconditionally: AWS is the default provider, and a plain
// `go build` links this package and nothing from any other cloud.
package aws

import (
	"context"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
	sdtypes "github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"

	"github.com/moriyoshi/muster/internal/provider"
)

// Name is the registered provider name.
const Name = "aws"

// healthFilters maps muster's neutral health tokens onto CloudMap's. It is an
// explicit table rather than a cast to the SDK enum so that each provider
// decides for itself which tokens it can honour, and one that cannot honour a
// filter says so instead of quietly widening it.
var healthFilters = map[string]sdtypes.HealthStatusFilter{
	provider.HealthHealthy:   sdtypes.HealthStatusFilterHealthy,
	provider.HealthUnhealthy: sdtypes.HealthStatusFilterUnhealthy,
	provider.HealthAll:       sdtypes.HealthStatusFilterAll,
	// CloudMap spells this HEALTHY_OR_ELSE_ALL; muster's token drops the "ELSE"
	// as one less AWS-ism in the script surface.
	provider.HealthHealthyOrAll: sdtypes.HealthStatusFilterHealthyOrElseAll,
}

// ServiceDiscovery resolves peers through CloudMap.
type ServiceDiscovery struct {
	svc       *servicediscovery.Client
	namespace string // default namespace
}

var _ provider.Discoverer = (*ServiceDiscovery)(nil)

func NewServiceDiscovery(svc *servicediscovery.Client, namespace string) *ServiceDiscovery {
	return &ServiceDiscovery{svc: svc, namespace: namespace}
}

// Discover performs a single CloudMap DiscoverInstances call and returns the
// matching instances. An empty result is returned as an empty slice, not an
// error — scripts retry with poll and decide how to handle emptiness.
// q.Namespace and q.Health override the defaults when non-empty.
func (sd *ServiceDiscovery) Discover(ctx context.Context, q provider.Query) ([]provider.Instance, error) {
	namespace := q.Namespace
	if namespace == "" {
		namespace = sd.namespace
	}
	hsf := sdtypes.HealthStatusFilterHealthy
	if q.Health != "" {
		f, ok := healthFilters[q.Health]
		if !ok {
			return nil, fmt.Errorf("invalid health status: %s", q.Health)
		}
		hsf = f
	}
	out, err := sd.svc.DiscoverInstances(ctx, &servicediscovery.DiscoverInstancesInput{
		NamespaceName: aws.String(namespace),
		ServiceName:   aws.String(q.Service),
		HealthStatus:  hsf,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to discover instances: %w", err)
	}
	entries := make([]provider.Instance, 0, len(out.Instances))
	for _, instance := range out.Instances {
		ipv4Addr, ipv6Addr, port := "", "", 0
		if v, ok := instance.Attributes["AWS_INSTANCE_IPV4"]; ok {
			ipv4Addr = v
		}
		if v, ok := instance.Attributes["AWS_INSTANCE_IPV6"]; ok {
			ipv6Addr = v
		}
		if v, ok := instance.Attributes["AWS_INSTANCE_PORT"]; ok {
			port, err = strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("failed to convert port to int: %w", err)
			}
		}
		entries = append(entries, provider.Instance{IPv4Addr: ipv4Addr, IPv6Addr: ipv6Addr, Port: port})
	}
	return entries, nil
}
