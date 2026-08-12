package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.starlark.net/starlark"
)

// taskMetadataStub serves the subset of the ECS task metadata endpoint muster
// reads: /task for the task document, and the container root for CreatedAt.
func taskMetadataStub(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/task", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"Cluster": "muster-e2e-tikv-main",
			"ServiceName": "tikv-pd",
			"TaskARN": "arn:aws:ecs:ap-northeast-1:123456789012:task/muster-e2e-tikv-main/abc123",
			"Family": "muster-e2e-tikv-tikv-pd",
			"Revision": "1",
			"AvailabilityZone": "ap-northeast-1a",
			"VPCID": "vpc-01234567890abcdef"
		}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"CreatedAt": "2026-08-11T15:00:00Z"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// ECS advertises the metadata endpoint in ECS_CONTAINER_METADATA_URI_V4. muster
// used to read AWS_CONTAINER_METADATA_URI_V4 — a name nothing sets, the
// AWS_CONTAINER_* family being the credentials variables — so metadata was
// silently unavailable on every ECS task. That took the TASK global and the
// no-argument form of all_ecs_tasks_running() with it, and because a script
// calling the latter raises on every resolve() attempt, the workload never
// started at all.
func TestFetchContainerMetadataEnvNames(t *testing.T) {
	for _, name := range []string{"ECS_CONTAINER_METADATA_URI_V4", "ECS_CONTAINER_METADATA_URI"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "")
			t.Setenv("ECS_CONTAINER_METADATA_URI", "")
			t.Setenv(name, taskMetadataStub(t))

			meta, err := fetchContainerMetadata(context.Background())
			if err != nil {
				t.Fatalf("fetchContainerMetadata: %v", err)
			}
			if meta.Cluster != "muster-e2e-tikv-main" {
				t.Errorf("Cluster = %q", meta.Cluster)
			}
			if meta.ServiceName != "tikv-pd" {
				t.Errorf("ServiceName = %q", meta.ServiceName)
			}
			if meta.CreatedAt != "2026-08-11T15:00:00Z" {
				t.Errorf("CreatedAt = %q, want the value from the container root endpoint", meta.CreatedAt)
			}
		})
	}
}

// v4 wins when both are present.
func TestFetchContainerMetadataPrefersV4(t *testing.T) {
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", taskMetadataStub(t))
	t.Setenv("ECS_CONTAINER_METADATA_URI", "http://127.0.0.1:1/should-not-be-used")

	if _, err := fetchContainerMetadata(context.Background()); err != nil {
		t.Fatalf("fetchContainerMetadata: %v", err)
	}
}

func TestFetchContainerMetadataOutsideECS(t *testing.T) {
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "")
	t.Setenv("ECS_CONTAINER_METADATA_URI", "")

	if _, err := fetchContainerMetadata(context.Background()); err == nil {
		t.Fatal("expected an error when no metadata endpoint is advertised")
	}
}

// The ECS builtins fall back to the task's own cluster/service, which is only
// possible when metadata resolved. This is the pairing that broke in practice.
func TestECSTargetFromTaskMetadata(t *testing.T) {
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", taskMetadataStub(t))

	meta, err := fetchContainerMetadata(context.Background())
	if err != nil {
		t.Fatalf("fetchContainerMetadata: %v", err)
	}
	v := joinValue(t, mustMain(t,
		`def main(): return go(lambda: "yes" if all_ecs_tasks_running() else "no")`,
		&engineDeps{ecs: &fakeECS{running: 3, desired: 3}, meta: meta}))
	if s, _ := starlark.AsString(v); s != "yes" {
		t.Fatalf("all_ecs_tasks_running() with metadata-derived target: got %v", v)
	}
}
