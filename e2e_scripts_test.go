package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"

	"github.com/moriyoshi/muster/internal/provider"
)

// The e2e stacks ship Starlark scripts that only ever run inside a Fargate task
// or a Cloud Run job. Loading them here keeps a syntax error or a renamed
// builtin from surviving until someone pays for a deployment to find out.

func loadE2ETiKVScript(t *testing.T, path string) *engine {
	t.Helper()
	return loadE2ETiKVScriptWithTarget(t, path, "test-cluster", "tikv-pd")
}

// loadE2ETiKVScriptWithTarget loads a script with the deployment's environment.
// An empty group/service models an instance whose metadata gave muster nothing
// to work with.
func loadE2ETiKVScriptWithTarget(t *testing.T, path, group, service string) *engine {
	t.Helper()
	t.Setenv("MUSTER_SUBNET_CIDR", "172.31.255.0/24")
	t.Setenv("MUSTER_PD_SERVICE", "tikv-pd")
	t.Setenv("MUSTER_PD_REPLICAS", "3")
	t.Setenv("MUSTER_SELF_GROUP", group)
	t.Setenv("MUSTER_SELF_SERVICE", service)

	eng, err := loadScript(context.Background(), filepath.FromSlash(path), &engineDeps{logger: testLogger()})
	if err != nil {
		t.Fatalf("loadScript(%s): %v", path, err)
	}
	return eng
}

// assertScriptRead checks that a global the script read from the environment
// actually holds what was set.
//
// Without this the environment-variable names here and in pd.star can drift
// apart silently: env() would return None, the script would take its
// "target unknown" path, and TestE2ETiKVLineupNeverRaises would keep passing
// while no longer exercising the failure it was written for.
func assertScriptRead(t *testing.T, eng *engine, global, want string) {
	t.Helper()
	got, ok := starlark.AsString(eng.globals[global])
	if !ok {
		t.Fatalf("%s is %v, want the value from the environment", global, eng.globals[global])
	}
	if got != want {
		t.Fatalf("%s = %q, want %q: the script and this test disagree on the env var name", global, got, want)
	}
}

func TestE2EScriptsLoad(t *testing.T) {
	for _, tc := range []struct {
		path  string
		funcs []string
	}{
		{
			path:  "e2e/tikv/aws/docker/tikv-pd/pd.star",
			funcs: []string{"resolve_pd", "pd_pre_stop", "pd_readiness", "pd_liveness", "lineup", "drop_member"},
		},
		{
			path:  "e2e/tikv/aws/docker/tikv-node/tikv.star",
			funcs: []string{"resolve_tikv", "tikv_readiness", "tikv_liveness"},
		},
		{
			path: "e2e/tikv/gcp/docker/tikv-pd/pd.star",
			funcs: []string{
				"resolve_pd", "pd_pre_stop", "pd_readiness", "pd_liveness",
				"lineup", "drop_member", "member_name", "pd_registered", "report",
			},
		},
		{
			path: "e2e/tikv/gcp/docker/tikv-node/tikv.star",
			funcs: []string{
				"resolve_tikv", "tikv_readiness", "tikv_liveness", "tikv_registered",
			},
		},
	} {
		t.Run(strings.TrimPrefix(tc.path, "e2e/"), func(t *testing.T) {
			eng := loadE2ETiKVScript(t, tc.path)
			for _, name := range tc.funcs {
				if _, ok := eng.globals[name].(*starlark.Function); !ok {
					t.Errorf("%s: %s is not a function", tc.path, name)
				}
			}
		})
	}
}

// The scripts derive PD's etcd member name from the task's own address, and it
// has to be a name etcd will take: no dots, stable for the member's lifetime.
func TestE2ETiKVPDMemberName(t *testing.T) {
	eng := loadE2ETiKVScript(t, "e2e/tikv/aws/docker/tikv-pd/pd.star")
	ctx := context.Background()

	for _, tc := range []struct{ ip, want string }{
		{"172.31.255.10", "pd-172-31-255-10"},
		{"10.0.0.1", "pd-10-0-0-1"},
	} {
		v, err := eng.invokeValue(ctx, eng.globals["member_name"], starlark.Tuple{starlark.String(tc.ip)})
		if err != nil {
			t.Fatalf("member_name(%s): %v", tc.ip, err)
		}
		if got, _ := starlark.AsString(v); got != tc.want {
			t.Errorf("member_name(%s) = %q, want %q", tc.ip, got, tc.want)
		}
	}
}

// lineup() only shaves churn off a cold start, but it runs inside resolve() —
// and Starlark cannot catch an error, so anything it raises escapes resolve(),
// aborts the attempt, and leaves PD permanently unstarted. It must therefore
// survive every way all_replicas_running() can fail.
//
// Both cases below were hit for real on Fargate: the first when muster looked
// for the metadata endpoint under the wrong environment variable and so never
// had a cluster/service to fall back on (see TestFetchContainerMetadataEnvNames),
// the second being the shape of any DescribeServices failure — AccessDenied
// from a mismatched IAM condition, throttling, a wrong cluster name.
func TestE2ETiKVLineupNeverRaises(t *testing.T) {
	for _, tc := range []struct {
		name           string
		group, service string
	}{
		{"target unknown", "", ""},
		{"describe-service fails", "muster-e2e-tikv-main", "tikv-pd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := loadE2ETiKVScriptWithTarget(t, "e2e/tikv/aws/docker/tikv-pd/pd.star", tc.group, tc.service)
			assertScriptRead(t, eng, "SELF_GROUP", tc.group)
			assertScriptRead(t, eng, "SELF_SERVICE", tc.service)

			// deps carries no replica-status capability, so the builtin errors as
			// soon as it is called — standing in for any API-level failure.
			if _, err := eng.invokeValue(context.Background(), eng.globals["lineup"], nil); err != nil {
				t.Fatalf("lineup() must never raise, got: %v", err)
			}
		})
	}
}

// A cold start hinges on live_peers() returning nothing when no peer answers;
// if it ever returned a bogus peer the replicas would try to --join a cluster
// that does not exist instead of racing for the seed lease. One peer is its own
// case: join() of a single promise resolves to a bare bool rather than a list.
func TestE2ETiKVLivePeersOnDeadPeers(t *testing.T) {
	eng := loadE2ETiKVScript(t, "e2e/tikv/aws/docker/tikv-pd/pd.star")

	for _, peers := range [][]string{
		nil,
		// 203.0.113.0/24 is TEST-NET-3: unroutable, so every probe fails.
		{"203.0.113.1"},
		{"203.0.113.1", "203.0.113.2"},
	} {
		arg := make([]starlark.Value, 0, len(peers))
		for _, p := range peers {
			arg = append(arg, starlark.String(p))
		}
		v, err := eng.invokeValue(context.Background(), eng.globals["live_peers"],
			starlark.Tuple{starlark.NewList(arg)})
		if err != nil {
			t.Fatalf("live_peers(%v): %v", peers, err)
		}
		list, ok := v.(*starlark.List)
		if !ok {
			t.Fatalf("live_peers(%v) returned %s, want list", peers, v.Type())
		}
		if list.Len() != 0 {
			t.Errorf("live_peers(%v) = %v, want []", peers, v)
		}
	}
}

// Both TiKV stacks derive PD's etcd member name from the replica's address,
// because neither Fargate nor Cloud Run has an identity that survives
// replacement. The name has to be one etcd will take: no dots, and stable for
// the lifetime of the member.
func TestE2ETiKVGCPMemberName(t *testing.T) {
	eng := loadGCPPDScript(t, &provider.Identity{Provider: "gcp", ID: "i-1", IPv4: "10.128.253.9"})

	v, err := eng.invokeValue(context.Background(), eng.globals["member_name"],
		starlark.Tuple{starlark.String("10.128.253.9")})
	if err != nil {
		t.Fatalf("member_name: %v", err)
	}
	if got, _ := starlark.AsString(v); got != "pd-10-128-253-9" {
		t.Errorf("member_name = %q, want pd-10-128-253-9", got)
	}
}

// SELF.ipv4 is empty on a Cloud Run service or job -- those support Direct VPC
// egress but not ingress -- so a PD deployed on one could never be reached by a
// peer. The script has to say that rather than starting a member nothing can
// talk to.
func TestE2ETiKVGCPRefusesWithoutAnAddress(t *testing.T) {
	eng := loadGCPPDScript(t, &provider.Identity{Provider: "gcp", ID: "i-1"})

	_, err := eng.invokeValue(context.Background(), eng.globals["me"], nil)
	if err == nil {
		t.Fatal("expected me() to fail when SELF.ipv4 is empty")
	}
	if !strings.Contains(err.Error(), "worker pool") {
		t.Errorf("the error should say which runtime is required, got: %v", err)
	}
}

// lineup() runs inside resolve(), and Starlark cannot catch an error, so
// anything it raises escapes resolve() and leaves PD permanently unstarted.
//
// The GCP version cannot use all_replicas_running() -- Cloud Run does not
// expose per-instance counts, so muster reports the capability unsupported and
// the builtin raises -- and counts what discovery can see instead. Here neither
// capability is configured, which is the worst case: both raise.
func TestE2ETiKVGCPLineupNeverRaises(t *testing.T) {
	eng := loadGCPPDScript(t, &provider.Identity{Provider: "gcp", ID: "i-1", IPv4: "10.128.253.9"})

	if _, err := eng.invokeValue(context.Background(), eng.globals["lineup"], nil); err != nil {
		t.Fatalf("lineup() must never raise, got: %v", err)
	}
}

func loadGCPPDScript(t *testing.T, self *provider.Identity) *engine {
	t.Helper()
	t.Setenv("MUSTER_PD_SERVICE", "tikv-pd")
	t.Setenv("MUSTER_PD_REPLICAS", "3")

	eng, err := loadScript(context.Background(),
		filepath.FromSlash("e2e/tikv/gcp/docker/tikv-pd/pd.star"),
		&engineDeps{logger: testLogger(), provider: "gcp", self: self})
	if err != nil {
		t.Fatalf("loadScript: %v", err)
	}
	return eng
}

// report() publishes each PD replica's own view of the cluster into the logs,
// and the Google Cloud suite's split-brain, quorum and store assertions are all
// reads of what it printed. So a report() that dies is not a degraded run — it
// is a suite that can only fail, and one whose failure says nothing about the
// cluster.
//
// It died. main() launches it with go(report) alongside spawn(), which means the
// first sample runs before PD is listening — resolve has a peer election to get
// through first. http_request raises on a refused connection, Starlark cannot
// catch a raise, and nothing joins the task, so the loop ended on its first
// iteration with the error discarded. Three replicas ran for eleven minutes and
// logged not one line, which reads exactly like a cluster with nothing to say.
//
// Nothing here listens on PD's port, so every sample fails the way it failed
// then. The loop must still be running when the context ends.
func TestE2ETiKVGCPReportSurvivesADeadPD(t *testing.T) {
	eng := loadE2ETiKVScript(t, "e2e/tikv/gcp/docker/tikv-pd/pd.star")

	// Long enough to get through a full round of samples, far short of the
	// 15s sleep that ends the first iteration.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := eng.invokeValue(ctx, eng.globals["report"], nil)
	if err == nil {
		t.Fatal("report() returned; it should still have been looping when the context ended")
	}
	// Reaching the sleep is the proof: the samples failed and the loop went on
	// anyway, so the only thing left to stop it was the deadline.
	if !errorMentions(err, "context") && !errorMentions(err, "cancelled") {
		t.Fatalf("report() died of %v, want the context deadline; a failed sample "+
			"must be skipped, not raised out of the loop", err)
	}
	if errorMentions(err, "connection refused") || errorMentions(err, "http_request") {
		t.Fatalf("report() was killed by one failed sample: %v", err)
	}
}

func errorMentions(err error, substr string) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), strings.ToLower(substr))
}

// me() is where both stacks answer "what address do I advertise to my peers?",
// and it was the last place the two scripts disagreed for no reason: the AWS
// pair picked the address off the interface with ifaddr(MUSTER_SUBNET_CIDR)
// while the Google Cloud pair read SELF.ipv4, which the AWS provider populates
// too (internal/provider/aws/identity.go). The variable was required, so a stack
// that forgot it failed at load time over a value muster already had.
//
// SELF.ipv4 is now the source on both, and MUSTER_SUBNET_CIDR is optional --
// which these scripts prove by loading without it.
func TestE2ETiKVAWSMeUsesSelfIPv4(t *testing.T) {
	for _, path := range []string{
		"e2e/tikv/aws/docker/tikv-pd/pd.star",
		"e2e/tikv/aws/docker/tikv-node/tikv.star",
	} {
		t.Run(strings.TrimPrefix(path, "e2e/tikv/aws/docker/"), func(t *testing.T) {
			t.Setenv("MUSTER_PD_SERVICE", "tikv-pd")
			t.Setenv("MUSTER_PD_REPLICAS", "3")
			// Deliberately unset: the point is that the script no longer needs it.
			t.Setenv("MUSTER_SUBNET_CIDR", "")

			self := &provider.Identity{IPv4: "172.31.255.42", Group: "c", Service: "tikv-pd"}
			eng, err := loadScript(context.Background(), filepath.FromSlash(path),
				&engineDeps{logger: testLogger(), provider: "aws", self: self})
			if err != nil {
				t.Fatalf("loadScript(%s) without MUSTER_SUBNET_CIDR: %v", path, err)
			}

			v, err := eng.invokeValue(context.Background(), eng.globals["me"], nil)
			if err != nil {
				t.Fatalf("me(): %v", err)
			}
			if got, _ := starlark.AsString(v); got != self.IPv4 {
				t.Errorf("me() = %q, want %q from SELF.ipv4", got, self.IPv4)
			}
		})
	}
}

// The fallback is the reason ifaddr() stayed: muster reads the task metadata
// once at startup, best-effort and without retry, so SELF can be empty on a
// platform that supports it. With neither source available me() must say so
// rather than advertise an address it invented.
func TestE2ETiKVAWSMeFailsWithNoAddressSource(t *testing.T) {
	t.Setenv("MUSTER_PD_SERVICE", "tikv-pd")
	t.Setenv("MUSTER_SUBNET_CIDR", "")

	eng, err := loadScript(context.Background(),
		filepath.FromSlash("e2e/tikv/aws/docker/tikv-pd/pd.star"),
		&engineDeps{logger: testLogger(), provider: "aws", self: nil})
	if err != nil {
		t.Fatalf("loadScript: %v", err)
	}
	_, err = eng.invokeValue(context.Background(), eng.globals["me"], nil)
	if err == nil {
		t.Fatal("me() returned an address with no SELF and no CIDR")
	}
	if !strings.Contains(err.Error(), "MUSTER_SUBNET_CIDR") {
		t.Errorf("me() failed without naming the missing configuration: %v", err)
	}
}
