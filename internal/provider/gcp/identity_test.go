//go:build gcp

package gcp

import (
	"context"
	"errors"
	"testing"
)

// fakeMetadata serves a canned metadata tree; a missing key errors, exactly as
// the metadata server does for an attribute that is not defined.
type fakeMetadata map[string]string

func (f fakeMetadata) GetWithContext(_ context.Context, suffix string) (string, error) {
	v, ok := f[suffix]
	if !ok {
		return "", errors.New("metadata: not defined")
	}
	return v, nil
}

func cloudRunMetadata() fakeMetadata {
	return fakeMetadata{
		"instance/id":                      "00bf4bf02d1d3e0f7c1a",
		"instance/region":                  "projects/1234567890/regions/asia-northeast1",
		"project/project-id":               "muster-e2e",
		"instance/network-interfaces/0/ip": "10.128.0.31",
	}
}

// clearRuntimeEnv unsets everything fetchIdentity reads, so a test starts from
// "not on Cloud Run" rather than from whatever the developer's shell has.
func clearRuntimeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		envService, envRevision, envConfiguration,
		envJob, envExecution, envTaskIndex, envTaskCount,
		envWorkerPool, envWorkerRev,
	} {
		t.Setenv(k, "")
	}
}

func TestFetchIdentityService(t *testing.T) {
	clearRuntimeEnv(t)
	t.Setenv(envService, "tikv-client")
	t.Setenv(envRevision, "tikv-client-00042-abc")
	t.Setenv(envConfiguration, "tikv-client")

	self, err := fetchIdentity(context.Background(), cloudRunMetadata())
	if err != nil {
		t.Fatalf("fetchIdentity: %v", err)
	}
	for _, c := range []struct{ field, got, want string }{
		{"Provider", self.Provider, "gcp"},
		{"ID", self.ID, "00bf4bf02d1d3e0f7c1a"},
		{"Service", self.Service, "tikv-client"},
		{"Region", self.Region, "asia-northeast1"},
		{"Group", self.Group, "asia-northeast1"},
		{`Extra["revision"]`, self.Extra["revision"], "tikv-client-00042-abc"},
		{`Extra["configuration"]`, self.Extra["configuration"], "tikv-client"},
		{`Extra["project"]`, self.Extra["project"], "muster-e2e"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}

	// The two deliberate blanks, both of which scripts branch on.
	if self.Name != "" {
		t.Errorf("Name = %q, want empty: a Cloud Run instance gets a fresh id on every scale event", self.Name)
	}
	// A service supports Direct VPC egress but not ingress, so the address it
	// sends from is not one anything can send to.
	if self.IPv4 != "" {
		t.Errorf("IPv4 = %q, want empty: a Cloud Run service instance accepts no inbound traffic", self.IPv4)
	}
	// Cloud Run's zone is the region with "-1" stuck on it, which is not a
	// zone. Reporting it would invite a script to place something by it.
	if self.Zone != "" {
		t.Errorf("Zone = %q, want empty: Cloud Run's zone is synthetic", self.Zone)
	}
	if self.CreatedAt != "" {
		t.Errorf("CreatedAt = %q, want empty", self.CreatedAt)
	}
}

func TestFetchIdentityJob(t *testing.T) {
	clearRuntimeEnv(t)
	t.Setenv(envJob, "seed-election")
	t.Setenv(envExecution, "seed-election-xk4mz")
	t.Setenv(envTaskIndex, "3")
	t.Setenv(envTaskCount, "12")

	self, err := fetchIdentity(context.Background(), cloudRunMetadata())
	if err != nil {
		t.Fatalf("fetchIdentity: %v", err)
	}
	if self.Service != "seed-election" {
		t.Errorf("Service = %q, want the job name", self.Service)
	}
	for _, c := range []struct{ key, want string }{
		{"job", "seed-election"},
		{"execution", "seed-election-xk4mz"},
		{"task_index", "3"},
		{"task_count", "12"},
	} {
		if got := self.Extra[c.key]; got != c.want {
			t.Errorf("Extra[%q] = %q, want %q", c.key, got, c.want)
		}
	}
	// A task index identifies a task within one execution, not across
	// executions, so it is not an identity and must not become one.
	if self.Name != "" {
		t.Errorf("Name = %q, want empty even for a job task", self.Name)
	}
}

// The lease owner only has to be unique -- Renew compares it for equality --
// and the instance id is. Two instances must never share one.
func TestIdentityIsUniquePerInstance(t *testing.T) {
	clearRuntimeEnv(t)
	t.Setenv(envService, "svc")

	a, err := fetchIdentity(context.Background(), cloudRunMetadata())
	if err != nil {
		t.Fatalf("fetchIdentity: %v", err)
	}
	other := cloudRunMetadata()
	other["instance/id"] = "11ce5cf13e2e4f1a8d2b"
	b, err := fetchIdentity(context.Background(), other)
	if err != nil {
		t.Fatalf("fetchIdentity: %v", err)
	}
	if a.ID == b.ID || a.ID == "" {
		t.Fatalf("two instances share the lease owner %q", a.ID)
	}
}

// A worker pool is the one Cloud Run runtime that supports Direct VPC
// *ingress*: each instance gets a private address on the VPC and other
// instances can reach it there. That is what makes a peer cluster possible, so
// SELF.ipv4 has to be populated -- and it is the only runtime where it is.
func TestFetchIdentityWorkerPool(t *testing.T) {
	clearRuntimeEnv(t)
	t.Setenv(envWorkerPool, "tikv-pd")
	t.Setenv(envWorkerRev, "tikv-pd.7")

	self, err := fetchIdentity(context.Background(), cloudRunMetadata())
	if err != nil {
		t.Fatalf("fetchIdentity: %v", err)
	}
	if self.IPv4 != "10.128.0.31" {
		t.Errorf("IPv4 = %q, want the instance's VPC address: a worker pool accepts inbound traffic", self.IPv4)
	}
	if self.Service != "tikv-pd" {
		t.Errorf("Service = %q, want the worker pool name", self.Service)
	}
	// The revision comes under CLOUD_RUN_REVISION here, not K_REVISION.
	if self.Extra["revision"] != "tikv-pd.7" {
		t.Errorf(`Extra["revision"] = %q, want tikv-pd.7`, self.Extra["revision"])
	}
	if self.Extra["worker_pool"] != "tikv-pd" {
		t.Errorf(`Extra["worker_pool"] = %q`, self.Extra["worker_pool"])
	}
	// Still no identity that survives replacement, addressable or not.
	if self.Name != "" {
		t.Errorf("Name = %q, want empty even on a worker pool", self.Name)
	}
}

// A job gets an address too, but nothing can reach it, so it must not be
// advertised. The distinction is per-runtime, not per-cloud.
func TestJobHasNoReachableAddress(t *testing.T) {
	clearRuntimeEnv(t)
	t.Setenv(envJob, "seed-election")

	self, err := fetchIdentity(context.Background(), cloudRunMetadata())
	if err != nil {
		t.Fatalf("fetchIdentity: %v", err)
	}
	if self.IPv4 != "" {
		t.Errorf("IPv4 = %q, want empty: a job instance accepts no inbound traffic", self.IPv4)
	}
}

func TestFetchIdentityOffCloudRun(t *testing.T) {
	clearRuntimeEnv(t)
	if _, err := fetchIdentity(context.Background(), cloudRunMetadata()); err == nil {
		t.Fatal("expected an error when none of the Cloud Run runtime variables is set")
	}
}

// The region falls back to parsing the synthetic zone on older revisions that
// do not serve instance/region.
func TestRegionFallsBackToZone(t *testing.T) {
	clearRuntimeEnv(t)
	t.Setenv(envService, "svc")

	md := cloudRunMetadata()
	delete(md, "instance/region")
	md["instance/zone"] = "projects/1234567890/zones/asia-northeast1-1"

	self, err := fetchIdentity(context.Background(), md)
	if err != nil {
		t.Fatalf("fetchIdentity: %v", err)
	}
	if self.Region != "asia-northeast1" {
		t.Errorf("Region = %q, want asia-northeast1", self.Region)
	}
}
