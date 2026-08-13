//go:build gcp

package gcp

import (
	"context"
	"testing"
	"time"

	"github.com/fsouza/fake-gcs-server/fakestorage"

	"github.com/moriyoshi/muster/internal/provider"
	"github.com/moriyoshi/muster/internal/provider/kvtest"
)

const testBucket = "muster-kv-test"

// newFakeGCS starts an in-process GCS. It is not a mock: it implements the
// generation preconditions the store's whole correctness rests on, which is the
// point -- a hand-written fake would just agree with whatever the store did.
func newFakeGCS(t *testing.T) *fakestorage.Server {
	t.Helper()
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme:         "http",
		InitialObjects: nil,
	})
	if err != nil {
		t.Fatalf("fake gcs: %v", err)
	}
	t.Cleanup(srv.Stop)
	srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: testBucket})
	return srv
}

// TestConformance is the one that matters. gcsKV expresses put-if-absent and
// compare-and-swap completely differently from the DynamoDB store -- generation
// preconditions rather than attribute conditions, two round trips rather than
// one -- so agreement on the semantics cannot be assumed from a reading.
func TestConformance(t *testing.T) {
	srv := newFakeGCS(t)
	client := srv.Client()

	kvtest.Run(t, kvtest.Config{KeyPrefix: "conf"}, func(owner string) provider.KVStore {
		return newGCSKV(client, testBucket, "", owner, "proj", "asia-northeast1")
	})
}

// Expiry is anchored on the object's server-side write time plus its ttl, not on
// a deadline the writer computed from its own clock. Two nodes whose clocks
// disagree therefore agree on when a lease lapses -- which the DynamoDB store,
// comparing a writer's absolute against a reader's clock, cannot promise.
func TestLeaseExpiryIgnoresTheWritersClock(t *testing.T) {
	srv := newFakeGCS(t)
	ctx := context.Background()

	// A writer an hour in the future would, on a writer-computed deadline, hold
	// the lease an hour longer than it asked for.
	skewed := newGCSKV(srv.Client(), testBucket, "", "skewed", "proj", "loc")
	skewed.now = func() time.Time { return time.Now().Add(time.Hour) }

	if ok, err := skewed.PutIfAbsent(ctx, "lease", "held", 300*time.Millisecond); err != nil || !ok {
		t.Fatalf("put: ok=%v err=%v", ok, err)
	}
	reader := newGCSKV(srv.Client(), testBucket, "", "reader", "proj", "loc")
	if _, present, _ := reader.Get(ctx, "lease"); !present {
		t.Fatal("lease should be live immediately after being taken")
	}

	time.Sleep(600 * time.Millisecond)
	if v, present, err := reader.Get(ctx, "lease"); err != nil || present {
		t.Fatalf("lease v=%q present=%v err=%v; a writer's clock skew must not extend it", v, present, err)
	}
	// And the lapsed lease is claimable, which is what lets a cluster recover
	// from a holder that died mid-bootstrap.
	if ok, err := reader.PutIfAbsent(ctx, "lease", "mine", time.Minute); err != nil || !ok {
		t.Fatalf("reclaim: ok=%v err=%v", ok, err)
	}
}

// Keys live under a single prefix so the bucket's lifecycle rule -- a janitor
// for leases nobody released -- cannot reap anything else sharing the bucket.
func TestKeysStayUnderTheLeasePrefix(t *testing.T) {
	srv := newFakeGCS(t)
	kv := newGCSKV(srv.Client(), testBucket, "cluster-a", "owner", "proj", "loc")

	if _, err := kv.PutIfAbsent(context.Background(), "seed", "v", 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	objs, _, err := srv.ListObjectsWithOptions(testBucket, fakestorage.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("expected one object, got %d", len(objs))
	}
	if want := leasePrefix + "/cluster-a/seed"; objs[0].Name != want {
		t.Fatalf("object name = %q, want %q", objs[0].Name, want)
	}
}
