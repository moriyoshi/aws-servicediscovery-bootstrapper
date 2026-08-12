//go:build gcp

package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	servicedirectory "cloud.google.com/go/servicedirectory/apiv1"
	"cloud.google.com/go/servicedirectory/apiv1/servicedirectorypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/moriyoshi/muster/internal/provider"
)

// maxEndpoints is the cap ResolveService accepts. Beyond it the response is
// silently truncated, so hitting it exactly is worth a warning.
const maxEndpoints = 100

// Discovery resolves peers through Service Directory.
type Discovery struct {
	client    *servicedirectory.LookupClient
	logger    *slog.Logger
	project   string
	location  string
	namespace string
}

var _ provider.Discoverer = (*Discovery)(nil)

func servicePath(project, location, namespace, service string) string {
	return fmt.Sprintf("projects/%s/locations/%s/namespaces/%s/services/%s",
		project, location, namespace, service)
}

// Discover performs one ResolveService lookup.
//
// Service Directory endpoints carry no health status and nothing reaps them, so
// only the ALL filter can be honoured. Asking for HEALTHY raises rather than
// quietly returning everything: a script that asked for healthy peers and was
// handed dead ones would join a cluster that is not there. The portable pattern
// is to take ALL and probe the peers yourself, which is what the shipped TiKV
// script already does.
func (d *Discovery) Discover(ctx context.Context, q provider.Query) ([]provider.Instance, error) {
	if q.Health != "" && q.Health != provider.HealthAll {
		return nil, fmt.Errorf(
			"health_status=%s: Service Directory endpoints carry no health status; use %q and probe the peers yourself",
			q.Health, provider.HealthAll)
	}
	namespace := q.Namespace
	if namespace == "" {
		namespace = d.namespace
	}

	out, err := d.client.ResolveService(ctx, &servicedirectorypb.ResolveServiceRequest{
		Name:         servicePath(d.project, d.location, namespace, q.Service),
		MaxEndpoints: maxEndpoints,
	})
	if err != nil {
		// A service with no endpoints registered yet is an empty result, not a
		// failure: scripts poll until peers appear, and turning a cold start
		// into an error would abort the attempt instead.
		if status.Code(err) == codes.NotFound {
			return []provider.Instance{}, nil
		}
		return nil, fmt.Errorf("failed to discover instances: %w", err)
	}

	endpoints := out.GetService().GetEndpoints()
	if len(endpoints) == maxEndpoints && d.logger != nil {
		d.logger.Warn("service directory result may be truncated",
			slog.String("service", q.Service), slog.Int("max_endpoints", maxEndpoints))
	}

	entries := make([]provider.Instance, 0, len(endpoints))
	for _, ep := range endpoints {
		e := provider.Instance{Port: int(ep.GetPort())}
		// An endpoint carries one address, not a v4/v6 pair as a CloudMap
		// instance does, so a dual-stack instance registers twice.
		if ip := net.ParseIP(ep.GetAddress()); ip != nil && ip.To4() != nil {
			e.IPv4Addr = ep.GetAddress()
		} else if ip != nil {
			e.IPv6Addr = ep.GetAddress()
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Registrar publishes this instance into Service Directory.
//
// It exists because nothing on Cloud Run does it: Service Directory
// auto-registration covers GKE Services only, and Cloud Run has no equivalent
// of ECS Service Connect. It is meaningful only on a **worker pool**, which is
// the one Cloud Run runtime whose instances are reachable at their VPC address
// -- services and jobs support Direct VPC egress but not ingress, so there is
// nothing to publish. Register refuses rather than advertising an address that
// nothing can connect to.
type Registrar struct {
	client    *servicedirectory.RegistrationClient
	logger    *slog.Logger
	project   string
	location  string
	namespace string

	// address is what gets published, and it also names the endpoint -- see
	// endpointID. It does not survive replacement, so a restarted instance does
	// not reclaim its own entry and the old one lingers until something removes
	// it. Hence deregister() from pre_stop, and hence scripts probing discovered
	// peers before trusting them.
	address string

	// registered is what we published, kept for Deregister.
	registered     string
	registeredAddr string
}

var _ provider.Registrar = (*Registrar)(nil)

// endpointID names the endpoint for an address.
//
// Not the instance id: Service Directory allows 63 characters and a Cloud Run
// instance id is around two hundred hex digits, so registering under it fails
// with "Invalid endpoint name" -- which is silent in effect, because a script
// only finds out when instances() returns nothing and every peer looks absent.
//
// The address is the right key anyway. It is what the entry publishes, it is
// unique among live instances on the network, and it is what the peers actually
// dial -- so a stale entry and a live one for the same address are the same
// endpoint, and Register overwriting it is correct rather than a collision.
func endpointID(address string) string {
	id := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, address)
	// Must begin with a letter, which an address never does.
	id = "ip-" + id
	if len(id) > 63 {
		id = id[:63]
	}
	return strings.TrimRight(id, "-")
}

func (r *Registrar) endpointPath(service, address string) string {
	return servicePath(r.project, r.location, r.namespace, service) + "/endpoints/" + endpointID(address)
}

func (r *Registrar) Register(ctx context.Context, reg provider.Registration) error {
	namespace := reg.Namespace
	if namespace == "" {
		namespace = r.namespace
	}
	address := reg.Address
	if address == "" {
		address = r.address
	}
	if address == "" {
		return fmt.Errorf("cannot register: this instance has no address peers can reach " +
			"(only a Cloud Run worker pool does; services and jobs support egress but not ingress)")
	}

	parent := servicePath(r.project, r.location, namespace, reg.Service)
	id := endpointID(address)
	// Clear any entry already under this address: it is either ours from a
	// previous attempt, or a predecessor's that outlived it. Either way the
	// address is now ours, and a retried call must not fail on ALREADY_EXISTS.
	_ = r.client.DeleteEndpoint(ctx, &servicedirectorypb.DeleteEndpointRequest{
		Name: parent + "/endpoints/" + id,
	})

	_, err := r.client.CreateEndpoint(ctx, &servicedirectorypb.CreateEndpointRequest{
		Parent:     parent,
		EndpointId: id,
		Endpoint: &servicedirectorypb.Endpoint{
			Address: address,
			Port:    int32(reg.Port),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to register endpoint: %w", err)
	}
	r.registered, r.registeredAddr = reg.Service, address
	if r.logger != nil {
		r.logger.Info("registered endpoint",
			slog.String("service", reg.Service), slog.String("endpoint", id),
			slog.String("address", address), slog.Int("port", reg.Port))
	}
	return nil
}

func (r *Registrar) Deregister(ctx context.Context) error {
	if r.registered == "" {
		return nil
	}
	err := r.client.DeleteEndpoint(ctx, &servicedirectorypb.DeleteEndpointRequest{
		Name: r.endpointPath(r.registered, r.registeredAddr),
	})
	// Already gone is the state we wanted.
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("failed to deregister endpoint: %w", err)
	}
	r.registered = ""
	return nil
}
