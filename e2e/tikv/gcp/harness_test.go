//go:build e2e_tikv_gcp

package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/moriyoshi/muster/e2e/internal/cloudrun"
	"github.com/moriyoshi/muster/e2e/internal/harness"
)

const (
	// envEnabled gates the whole thing: this test provisions billable Google
	// Cloud infrastructure, so it never runs by accident.
	envEnabled = "MUSTER_E2E_TIKV_GCP"
	// envProvision=0 asserts against a stack that is already up (`make up`).
	envProvision = "MUSTER_E2E_TIKV_GCP_PROVISION"
	// envKeep=1 leaves the stack running after a failure, for post-mortems.
	envKeep = "MUSTER_E2E_TIKV_GCP_KEEP"
)

type stack struct {
	project   string
	region    string
	prefix    string
	bucket    string
	namespace string

	pdPool      string
	tikvPool    string
	pdDiscovery string
	tikvService string

	pdWant   int
	tikvWant int
}

func setupStack(t *testing.T) *stack {
	t.Helper()

	if os.Getenv(envEnabled) != "1" {
		t.Skipf("set %s=1 to run the Google Cloud TiKV end-to-end test (it creates billable resources)", envEnabled)
	}
	harness.RequireTools(t, "gcloud", "make", "terraform", "docker")

	if os.Getenv(envProvision) != "0" {
		// Teardown is registered before provisioning, so a failure part-way
		// through still tears down what was created. Log collection is
		// registered after, so LIFO reads the logs before destroy removes the
		// pools that produced them.
		t.Cleanup(func() {
			if os.Getenv(envKeep) == "1" {
				t.Logf("%s=1: leaving the stack up; run `make destroy` when done", envKeep)
				return
			}
			if err := harness.MakeTarget(t, "destroy"); err != nil {
				t.Errorf("make destroy: %v (resources may still be billing)", err)
			}
		})
		if err := harness.MakeTarget(t, "up"); err != nil {
			t.Fatalf("make up: %v", err)
		}
	}

	out := harness.ReadTerraformOutputs(t)
	s := &stack{
		project:     out.Str(t, "project"),
		region:      out.Str(t, "region"),
		prefix:      out.Str(t, "name_prefix"),
		bucket:      out.Str(t, "kv_bucket"),
		namespace:   out.Str(t, "namespace_name"),
		pdPool:      out.Str(t, "pd_pool"),
		tikvPool:    out.Str(t, "tikv_pool"),
		pdDiscovery: out.Str(t, "pd_discovery_name"),
		tikvService: out.Str(t, "tikv_discovery_name"),
		pdWant:      out.Int(t, "pd_instance_count"),
		tikvWant:    out.Int(t, "tikv_instance_count"),
	}

	t.Cleanup(func() {
		if t.Failed() {
			s.dumpLogs(t, s.pdPool, 300)
			s.dumpLogs(t, s.tikvPool, 200)
		}
	})
	return s
}

// --- gcloud ----------------------------------------------------------------

func (s *stack) gcloud(ctx context.Context, args ...string) (string, error) {
	args = append(args, "--project="+s.project)
	cmd := exec.CommandContext(ctx, "gcloud", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("gcloud %s: %v: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (s *stack) gcloudJSON(ctx context.Context, into any, args ...string) error {
	args = append(args, "--format=json")
	out, err := s.gcloud(ctx, args...)
	if err != nil {
		return err
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal([]byte(out), into)
}

// --- the pools -------------------------------------------------------------

// poolReady reports whether a pool has reconciled onto a ready revision.
//
// The parsing lives in e2e/internal/cloudrun so that it can be tested without a
// project, which is the whole reason this was broken: it read the Cloud Run v2
// API's field names -- terminalCondition.state, the spelling the Terraform
// provider uses -- out of gcloud's output, which is Knative-shaped. Those keys
// are simply absent there, so the struct stayed zero and the check reported
// "condition  is :" while waiting out its full fifteen minutes on two pools
// that had been ready for most of it.
func (s *stack) poolReady(ctx context.Context, pool string) error {
	raw, err := s.describePool(ctx, pool)
	if err != nil {
		return err
	}
	return cloudrun.PoolReady(pool, raw)
}

func (s *stack) describePool(ctx context.Context, pool string) ([]byte, error) {
	out, err := s.gcloud(ctx, "beta", "run", "worker-pools", "describe", pool,
		"--region="+s.region, "--format=json")
	return []byte(out), err
}

// --- logs, which are how the cluster is observed ---------------------------
//
// Nothing outside the VPC can query PD: a worker pool instance is reachable
// from the network and nowhere else, and this stack has no bastion. So each
// replica reports its *own* view on a loop and the assertions read those back.
//
// That is not a workaround for the missing route -- it is what makes the
// split-brain check exhaustive. Asking one replica through a load balancer
// could never see two clusters; asking every replica about itself is exactly
// what the ECS suite achieves by shelling into each task.

// logEntries reads a pool's Cloud Logging entries, newest last.
//
// Two things here were wrong at once, and both failed by returning nothing
// rather than by erroring. The filter named resource type `cloud_run_worker`,
// which does not exist — Cloud Logging answers an unknown type with an empty
// result set, not a complaint. And the output was read as `value(textPayload)`,
// which is empty for every line muster writes: muster logs structured JSON, the
// agent parses it into jsonPayload, and textPayload carries only what the
// workload itself printed. So the self-reports the whole suite reads back were
// unreachable twice over, and `PDClusterBootstrapped` sat for its full twenty
// minutes saying no replica had reported a cluster while three of them were.
//
// The parsing lives in e2e/internal/cloudrun, where a captured entry pins the
// resource type and the two payload shapes against real data. It could not be
// tested here.
// An empty revision reads every revision's entries, which is what a failure dump
// wants and what an assertion must not have.
func (s *stack) logEntries(ctx context.Context, pool, revision string, freshness time.Duration, limit int) ([]cloudrun.LogEntry, error) {
	filter := fmt.Sprintf(`resource.type=%q AND resource.labels.worker_pool_name=%q`,
		cloudrun.WorkerPoolResourceType, pool)
	if revision != "" {
		filter += fmt.Sprintf(" AND resource.labels.%s=%q", cloudrun.RevisionLabel, revision)
	}
	out, err := s.gcloud(ctx, "logging", "read", filter,
		// Newest first, and the limit is the reason. PD is loud -- thousands of
		// lines an hour -- so `--limit` truncates any useful window, and
		// ascending order truncates it at the wrong end: the first read of a
		// live cluster came back with four thousand entries from half past
		// eight in the morning and not one self-report in them.
		"--order=desc",
		fmt.Sprintf("--freshness=%dm", int(freshness.Minutes())),
		fmt.Sprintf("--limit=%d", limit),
		"--format=json")
	if err != nil {
		return nil, err
	}
	return cloudrun.ParseEntries([]byte(out))
}

// latestReports returns the most recent record of the given kind per replica of
// the revision the pool has settled on.
//
// Scoping to that revision is not tidiness. A pool's logs outlive its
// instances, so an hour-wide window holds replicas from revisions that no
// longer exist -- and those reported a different cluster id, which is precisely
// what NoSplitBrain is looking for. Read without the scope, a healthy pool
// reports five replicas and two clusters.
func (s *stack) latestReports(ctx context.Context, pool, msg string) (map[string]cloudrun.Report, error) {
	raw, err := s.describePool(ctx, pool)
	if err != nil {
		return nil, err
	}
	revision, err := cloudrun.ReadyRevision(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", pool, err)
	}
	entries, err := s.logEntries(ctx, pool, revision, 60*time.Minute, 4000)
	if err != nil {
		return nil, err
	}
	return cloudrun.LatestReports(entries, msg), nil
}

// dumpLogs prints muster's own decisions first and the workload's output after.
//
// The order is the point. A flat tail is dominated by the workload -- PD alone
// writes thousands of lines an hour, so the last three hundred cover about a
// minute and contain nothing muster did. Every line muster wrote across the
// whole window fits in a fraction of that, and it is the half that explains
// which branch a replica took.
func (s *stack) dumpLogs(t *testing.T, pool string, limit int) {
	t.Helper()
	// Deliberately unscoped: after a failure you want every generation, not the
	// one that happens to be current.
	entries, err := s.logEntries(context.Background(), pool, "", 60*time.Minute, 4000)
	if err != nil {
		t.Logf("could not read logs for %s: %v", pool, err)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp < entries[j].Timestamp })

	decisions := cloudrun.Decisions(entries)
	t.Logf("--- %s: what muster decided (%d lines) ---", pool, len(decisions))
	for _, e := range decisions {
		t.Logf("  | %s", e)
	}

	workload := cloudrun.Workload(entries)
	if len(workload) > limit {
		workload = workload[len(workload)-limit:]
	}
	t.Logf("--- %s: last %d lines from the workload ---", pool, len(workload))
	for _, e := range workload {
		t.Logf("  | %s", e)
	}
}

// --- PD API shapes ---------------------------------------------------------

type pdClusterInfo struct {
	ID           int64 `json:"id"`
	MaxPeerCount int   `json:"max_peer_count"`
}

type pdMembers struct {
	Members []struct {
		Name       string   `json:"name"`
		ClientURLs []string `json:"client_urls"`
	} `json:"members"`
}

func (m pdMembers) names() []string {
	out := make([]string, 0, len(m.Members))
	for _, member := range m.Members {
		out = append(out, member.Name)
	}
	sort.Strings(out)
	return out
}

type pdStores struct {
	Count  int `json:"count"`
	Stores []struct {
		Store struct {
			ID        int64  `json:"id"`
			Address   string `json:"address"`
			StateName string `json:"state_name"`
		} `json:"store"`
	} `json:"stores"`
}

// --- service directory -----------------------------------------------------

type sdEndpoint struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// endpoints lists what the instances registered themselves as. Every one was
// written by muster's register(): nothing on Cloud Run registers an instance,
// so unlike the CloudMap check on the AWS side this tests muster's own code.
func (s *stack) endpoints(ctx context.Context, service string) ([]sdEndpoint, error) {
	var out []sdEndpoint
	if err := s.gcloudJSON(ctx, &out, "service-directory", "endpoints", "list",
		"--location="+s.region, "--namespace="+s.namespace, "--service="+service); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// --- kv --------------------------------------------------------------------

// seedLease reads the object muster's kv_put_if_absent wrote, which is the
// artefact the cold-start election leaves behind. Empty and no error means the
// key is absent, which is what a released lease looks like.
func (s *stack) seedLease(ctx context.Context) (string, error) {
	out, err := s.gcloud(ctx, "storage", "cat",
		fmt.Sprintf("gs://%s/leases/tikv-pd/seed", s.bucket))
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "No such object") {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// memberName mirrors member_name() in pd.star: PD's member names are derived
// from the address because a Cloud Run instance has no identity that outlives
// it. Assertions that compare membership to the registered replicas have to
// spell it the same way.
func memberName(addr string) string { return "pd-" + strings.ReplaceAll(addr, ".", "-") }
