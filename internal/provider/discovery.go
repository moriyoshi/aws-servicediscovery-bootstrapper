package provider

import "context"

// Instance is one discovered peer.
type Instance struct {
	IPv4Addr string
	IPv6Addr string
	Port     int
}

// Query is a single discovery lookup. Empty fields mean "the provider's
// default", which is what -namespace and the HEALTHY filter supply today.
type Query struct {
	Namespace string
	Service   string
	// Health filters by health status. The neutral layer does not validate it;
	// each provider accepts the tokens it can honour and rejects the rest --
	// never silently widening a HEALTHY request into "everything", which would
	// hand a script dead peers it explicitly asked not to see.
	Health string
}

// Health filter tokens. Providers need not support all of them.
const (
	HealthHealthy   = "HEALTHY"
	HealthUnhealthy = "UNHEALTHY"
	HealthAll       = "ALL"
	// HealthHealthyOrAll returns healthy instances, or every instance when none
	// is healthy.
	HealthHealthyOrAll = "HEALTHY_OR_ALL"
)

type Discoverer interface {
	// Discover performs one lookup with no internal retry. No matches is an
	// empty slice and a nil error -- scripts poll and decide what emptiness
	// means for them.
	Discover(ctx context.Context, q Query) ([]Instance, error)
}

// Registrar publishes this instance into the service registry, so peers can
// discover it. It exists because not every platform does it for you.
type Registrar interface {
	Register(ctx context.Context, r Registration) error
	Deregister(ctx context.Context) error
}

// Registration is one endpoint to publish. Empty Namespace means the provider's
// default; empty Address means the instance's own address from its Identity.
type Registration struct {
	Namespace string
	Service   string
	Address   string
	Port      int
}
