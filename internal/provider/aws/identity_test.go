package aws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
// silently unavailable on every ECS task. That took the identity global and the
// no-argument form of the replica-status builtin with it, and because a script
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

// The mapping onto the neutral Identity is what the rest of muster sees, so it
// is pinned field by field rather than trusted to stay right by inspection.
func TestFetchIdentityMapping(t *testing.T) {
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", taskMetadataStub(t))

	self, err := FetchIdentity(context.Background())
	if err != nil {
		t.Fatalf("FetchIdentity: %v", err)
	}

	const arn = "arn:aws:ecs:ap-northeast-1:123456789012:task/muster-e2e-tikv-main/abc123"
	for _, c := range []struct{ field, got, want string }{
		{"Provider", self.Provider, "aws"},
		{"ID", self.ID, arn},
		{"Group", self.Group, "muster-e2e-tikv-main"},
		{"Service", self.Service, "tikv-pd"},
		{"Zone", self.Zone, "ap-northeast-1a"},
		{"Region", self.Region, "ap-northeast-1"},
		{"Network", self.Network, "vpc-01234567890abcdef"},
		{"CreatedAt", self.CreatedAt, "2026-08-11T15:00:00Z"},
		{`Extra["family"]`, self.Extra["family"], "muster-e2e-tikv-tikv-pd"},
		{`Extra["revision"]`, self.Extra["revision"], "1"},
		{`Extra["task_arn"]`, self.Extra["task_arn"], arn},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}

	// A Fargate task has no identity stable across replacement, and scripts read
	// the emptiness as "derive a name some other way". Filling it in would hand
	// them a member name that silently changes on every replacement.
	if self.Name != "" {
		t.Errorf("Name = %q, want empty: ECS tasks have no stable name", self.Name)
	}
}

func TestRegionFromTaskARN(t *testing.T) {
	cases := map[string]string{
		"arn:aws:ecs:ap-northeast-1:123456789012:task/c/abc": "ap-northeast-1",
		"arn:aws-cn:ecs:cn-north-1:123456789012:task/c/abc":  "cn-north-1",
		"not-an-arn":  "",
		"":            "",
		"arn:aws:ecs": "",
	}
	for in, want := range cases {
		if got := regionFromTaskARN(in); got != want {
			t.Errorf("regionFromTaskARN(%q) = %q, want %q", in, got, want)
		}
	}
}
