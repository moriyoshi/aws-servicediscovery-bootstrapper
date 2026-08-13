//go:build gcp_live

package gcp

// A run of the kv conformance suite against a real Cloud Storage bucket.
//
// The fake in kv_test.go implements generation preconditions, but it is
// single-process and cannot reproduce contention, retries, or the per-object
// write rate. The kv store is also the only part of this provider whose
// correctness is subtle, so it is worth a few cents to see it work for real
// before trusting it with a seed election.
//
// Run with:
//
//	MUSTER_GCP_KV_BUCKET=my-bucket \
//	  go test -tags=gcp,gcp_live ./internal/provider/gcp/
//
// Both tags are needed: gcp_live gates this file, and gcp compiles the provider
// it exercises. Credentials come from Application Default Credentials. The
// bucket must already exist; the suite writes only under leases/, and removes
// what it wrote.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/moriyoshi/muster/internal/provider"
	"github.com/moriyoshi/muster/internal/provider/kvtest"
)

func TestLiveGCSConformance(t *testing.T) {
	bucket := os.Getenv("MUSTER_GCP_KV_BUCKET")
	if bucket == "" {
		t.Skip("set MUSTER_GCP_KV_BUCKET to run against a real bucket")
	}
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatalf("storage client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// Namespaced per run: the bucket outlives the test, and a leftover
	// permanent key would fail the next run's put-if-absent.
	prefix := fmt.Sprintf("live-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanup(t, client, bucket, prefix) })

	kvtest.Run(t, kvtest.Config{
		KeyPrefix: prefix,
		// Comfortably above the round-trip to a real bucket, so a slow call
		// cannot expire a lease the test still expects to be live.
		LeaseTTL: 5 * time.Second,
	}, func(owner string) provider.KVStore {
		return newGCSKV(client, bucket, "", owner, "", "")
	})
}

func cleanup(t *testing.T, client *storage.Client, bucket, prefix string) {
	t.Helper()
	ctx := context.Background()
	it := client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: leasePrefix + "/" + prefix})
	for {
		attrs, err := it.Next()
		if err != nil {
			return
		}
		if err := client.Bucket(bucket).Object(attrs.Name).Delete(ctx); err != nil {
			t.Logf("cleanup %s: %v", attrs.Name, err)
		}
	}
}
