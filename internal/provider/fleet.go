package provider

import "context"

// WorkloadRef names a replica set. A struct rather than two strings so a
// provider needing a third coordinate can gain a field without breaking the
// others.
type WorkloadRef struct {
	// Group is the scheduling group: an ECS cluster, a GCE managed instance
	// group's location. Empty means this instance's own.
	Group string
	// Name is the replica set within it: an ECS service, a MIG. Empty means
	// this instance's own.
	Name string
}

type Fleet interface {
	// AllReplicasRunning reports whether the orchestrator has every desired
	// replica of ref running. Point-in-time; scripts poll it.
	//
	// It is the scheduler's view, not "the workload inside is up" -- which is
	// why scripts treat it as advisory and probe peers themselves.
	AllReplicasRunning(ctx context.Context, ref WorkloadRef) (bool, error)
}
