package main

import (
	"context"
	"path/filepath"
	"testing"

	"go.starlark.net/starlark"
)

// The e2e/tikv stack ships two Starlark scripts that only ever run inside a
// Fargate task. Loading them here keeps a syntax error or a renamed builtin
// from surviving until someone pays for an AWS deployment to find out.

func loadE2ETiKVScript(t *testing.T, path string) *engine {
	t.Helper()
	return loadE2ETiKVScriptWithECS(t, path, "test-cluster", "tikv-pd")
}

// loadE2ETiKVScriptWithECS loads a script with the deployment's environment.
// Empty cluster/service model a task whose metadata endpoint gave muster
// nothing to work with.
func loadE2ETiKVScriptWithECS(t *testing.T, path, cluster, service string) *engine {
	t.Helper()
	t.Setenv("MUSTER_SUBNET_CIDR", "172.31.255.0/24")
	t.Setenv("MUSTER_PD_SERVICE", "tikv-pd")
	t.Setenv("MUSTER_PD_REPLICAS", "3")
	t.Setenv("MUSTER_ECS_CLUSTER", cluster)
	t.Setenv("MUSTER_ECS_SERVICE", service)

	eng, err := loadScript(context.Background(), filepath.FromSlash(path), &engineDeps{logger: testLogger()})
	if err != nil {
		t.Fatalf("loadScript(%s): %v", path, err)
	}
	return eng
}

func TestE2ETiKVScriptsLoad(t *testing.T) {
	for _, tc := range []struct {
		path  string
		funcs []string
	}{
		{
			path:  "e2e/tikv/docker/tikv-pd/pd.star",
			funcs: []string{"resolve_pd", "pd_pre_stop", "pd_readiness", "pd_liveness", "lineup", "drop_member"},
		},
		{
			path:  "e2e/tikv/docker/tikv-node/tikv.star",
			funcs: []string{"resolve_tikv", "tikv_readiness", "tikv_liveness"},
		},
	} {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
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
	eng := loadE2ETiKVScript(t, "e2e/tikv/docker/tikv-pd/pd.star")
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
// survive every way all_ecs_tasks_running() can fail.
//
// Both cases below were hit for real on Fargate: the first when muster looked
// for the metadata endpoint under the wrong environment variable and so never
// had a cluster/service to fall back on (see TestFetchContainerMetadataEnvNames),
// the second being the shape of any DescribeServices failure — AccessDenied
// from a mismatched IAM condition, throttling, a wrong cluster name.
func TestE2ETiKVLineupNeverRaises(t *testing.T) {
	for _, tc := range []struct {
		name             string
		cluster, service string
	}{
		{"target unknown", "", ""},
		{"DescribeServices fails", "muster-e2e-tikv-main", "tikv-pd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := loadE2ETiKVScriptWithECS(t, "e2e/tikv/docker/tikv-pd/pd.star", tc.cluster, tc.service)

			// deps carries no ECS client, so the builtin errors as soon as it is
			// called — standing in for any API-level failure.
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
	eng := loadE2ETiKVScript(t, "e2e/tikv/docker/tikv-pd/pd.star")

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
