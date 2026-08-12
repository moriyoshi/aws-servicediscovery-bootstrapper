//go:build gcp

package gcp

import (
	"context"
	"fmt"
	"os"
	"strings"

	"cloud.google.com/go/compute/metadata"

	"github.com/moriyoshi/muster/internal/provider"
)

// Cloud Run's own environment variables. A service gets K_SERVICE/K_REVISION/
// K_CONFIGURATION; a job gets CLOUD_RUN_JOB and, per task, an index and a
// count; a worker pool gets CLOUD_RUN_WORKER_POOL and CLOUD_RUN_REVISION.
const (
	envService       = "K_SERVICE"
	envRevision      = "K_REVISION"
	envConfiguration = "K_CONFIGURATION"
	envJob           = "CLOUD_RUN_JOB"
	envExecution     = "CLOUD_RUN_EXECUTION"
	envTaskIndex     = "CLOUD_RUN_TASK_INDEX"
	envTaskCount     = "CLOUD_RUN_TASK_COUNT"
	envWorkerPool    = "CLOUD_RUN_WORKER_POOL"
	envWorkerRev     = "CLOUD_RUN_REVISION"
)

// RuntimeEnv lists the variables whose presence means we are on Cloud Run, for
// provider autodetection.
func RuntimeEnv() []string { return []string{envService, envJob, envWorkerPool} }

// metadataClient is the subset of the GCE metadata client this package uses, so
// tests can supply one without a metadata server.
type metadataClient interface {
	GetWithContext(ctx context.Context, suffix string) (string, error)
}

// lastSegment returns the part after the final "/", which is how the metadata
// server returns most references.
func lastSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// regionOfZone turns a zone into its region: names are <region>-<letter>, so
// the region is everything before the final dash.
func regionOfZone(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return ""
}

// get reads one metadata key, treating any failure as an empty value: all of
// this is optional, and the whole point is to be best-effort.
func get(ctx context.Context, c metadataClient, suffix string) string {
	v, err := c.GetWithContext(ctx, suffix)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

func putIf(m map[string]string, key, val string) {
	if val != "" {
		m[key] = val
	}
}

// fetchIdentity reads Cloud Run's environment and metadata server, and maps
// them onto the neutral Identity.
//
// Name is always empty: Cloud Run has no identity that survives replacement, on
// any of its three runtimes -- an instance gets a fresh id on every scale event.
// Scripts read the emptiness as "derive a name some other way" rather than being
// handed one that changes underneath them. Fargate is empty for the same reason.
//
// IPv4 depends on the runtime, and the difference decides whether a script can
// build a peer cluster at all:
//
//	worker pool  the instance's own address on your VPC, and reachable there.
//	             Direct VPC *ingress* is supported, so other instances -- and
//	             anything else on the network -- can connect to it.
//	service/job  empty. Only Direct VPC egress is supported, so the address the
//	             instance sends from is not one anything can send to, and
//	             publishing it as SELF.ipv4 would read as "connect to me here".
func fetchIdentity(ctx context.Context, c metadataClient) (*provider.Identity, error) {
	service := os.Getenv(envService)
	job := os.Getenv(envJob)
	pool := os.Getenv(envWorkerPool)
	if service == "" && job == "" && pool == "" {
		return nil, fmt.Errorf("none of %s, %s or %s is set; not running on Cloud Run?",
			envService, envJob, envWorkerPool)
	}

	id := get(ctx, c, "instance/id")

	// A Cloud Run instance's zone is synthetic -- the region with "-1" stuck on
	// the end -- so only the region is real, and only the region is exposed.
	region := lastSegment(get(ctx, c, "instance/region"))
	if region == "" {
		region = regionOfZone(lastSegment(get(ctx, c, "instance/zone")))
	}

	self := &provider.Identity{
		Provider: Name,
		// Unique per instance, which is all a kv lease owner has to be. It is
		// explicitly not stable: a new instance is a new owner, which is the
		// correct reading -- it never held the previous one's lease.
		ID:      id,
		Service: service,
		Region:  region,
		// Group pairs with Service to address a replica set. Cloud Run has no
		// API that answers questions about one, so this exists only to give
		// scripts somewhere consistent to read the region from.
		Group: region,
		Extra: map[string]string{},
	}
	switch {
	case pool != "":
		self.Service = pool
		// Only a worker pool has an address anything can dial. See above.
		self.IPv4 = get(ctx, c, "instance/network-interfaces/0/ip")
	case job != "":
		self.Service = job
	}

	putIf(self.Extra, "instance_id", id)
	putIf(self.Extra, "project", get(ctx, c, "project/project-id"))
	putIf(self.Extra, "service", service)
	putIf(self.Extra, "revision", os.Getenv(envRevision))
	putIf(self.Extra, "configuration", os.Getenv(envConfiguration))
	putIf(self.Extra, "job", job)
	putIf(self.Extra, "execution", os.Getenv(envExecution))
	// A job's tasks are numbered, which is the closest thing Cloud Run has to
	// an ordinal. It identifies a task within one execution, not across
	// executions, so it is not an identity -- but it is the natural thing to
	// log with, and to fan work out by.
	putIf(self.Extra, "task_index", os.Getenv(envTaskIndex))
	putIf(self.Extra, "task_count", os.Getenv(envTaskCount))
	putIf(self.Extra, "worker_pool", pool)
	if pool != "" {
		// A worker pool revision comes under its own name, not K_REVISION.
		putIf(self.Extra, "revision", os.Getenv(envWorkerRev))
	}

	// CreatedAt is left empty: the metadata server does not carry it.
	return self, nil
}

// realMetadata adapts the package-level metadata client.
type realMetadata struct{ c *metadata.Client }

func (m realMetadata) GetWithContext(ctx context.Context, suffix string) (string, error) {
	return m.c.GetWithContext(ctx, suffix)
}
