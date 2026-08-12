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
	out, err := s.gcloud(ctx, "beta", "run", "worker-pools", "describe", pool,
		"--region="+s.region, "--format=json")
	if err != nil {
		return err
	}
	return cloudrun.PoolReady(pool, []byte(out))
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

// report is one line of a replica's self-report, as muster logged it.
type report struct {
	Msg  string `json:"msg"`
	Who  string `json:"who"`
	Body string `json:"body"`
}

func (s *stack) logLines(ctx context.Context, pool string, freshness time.Duration, limit int) ([]string, error) {
	filter := fmt.Sprintf(
		`resource.type=cloud_run_worker AND resource.labels.worker_pool_name=%q`, pool)
	out, err := s.gcloud(ctx, "logging", "read", filter,
		"--order=asc",
		fmt.Sprintf("--freshness=%dm", int(freshness.Minutes())),
		fmt.Sprintf("--limit=%d", limit),
		"--format=value(textPayload)")
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// latestReports returns the most recent record of the given kind per replica.
// Latest, not first: a replica's early reports are of a cluster still forming,
// and asserting on those would be asserting on a moment rather than a state.
func (s *stack) latestReports(ctx context.Context, pool, msg string) (map[string]report, error) {
	lines, err := s.logLines(ctx, pool, 60*time.Minute, 4000)
	if err != nil {
		return nil, err
	}
	out := map[string]report{}
	for _, line := range lines {
		var r report
		if json.Unmarshal([]byte(line), &r) != nil || r.Msg != msg {
			continue
		}
		out[r.Who] = r // ascending order, so the last write wins
	}
	return out, nil
}

func (s *stack) dumpLogs(t *testing.T, pool string, limit int) {
	t.Helper()
	lines, err := s.logLines(context.Background(), pool, 60*time.Minute, limit)
	if err != nil {
		t.Logf("could not read logs for %s: %v", pool, err)
		return
	}
	t.Logf("--- last %d log lines for %s ---", limit, pool)
	for _, l := range lines {
		t.Logf("  | %s", l)
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
