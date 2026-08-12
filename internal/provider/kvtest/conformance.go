// Package kvtest is the shared conformance suite for provider.KVStore.
//
// Seed election is the one thing in muster that a second implementation can
// break silently: a store with subtly different conditional-write semantics
// still returns plausible booleans, and the failure only shows up as two
// clusters where there should be one. Every implementation runs this suite, so
// the semantics live in one place rather than in each backend's own tests.
package kvtest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moriyoshi/muster/internal/provider"
)

// NewStore returns a store for owner. Stores returned by successive calls MUST
// share their backing state, because that is how the suite tests lease
// ownership -- two owners, one store.
type NewStore func(owner string) provider.KVStore

// Config parameterises the suite for one backend.
type Config struct {
	// KeyPrefix namespaces the keys, so a run against a shared real store does
	// not collide with another run or another suite.
	KeyPrefix string

	// LeaseTTL is how long the suite's leases live. It has to exceed the
	// backend's expiry granularity: DynamoDB stores expires_at in whole
	// seconds, so a sub-second lease there rounds to "already expired" and the
	// suite would be testing nothing. It also has to be long enough that a busy
	// CI machine cannot expire a lease mid-assertion. Defaults to 300ms, which
	// suits an in-process store.
	//
	// Expiry is exercised with real time rather than an injected clock: the
	// suite has to run unchanged against a remote store, where the clock that
	// decides is not ours to fake.
	LeaseTTL time.Duration
}

const defaultLeaseTTL = 300 * time.Millisecond

// Run executes the suite against newStore.
func Run(t *testing.T, cfg Config, newStore NewStore) {
	t.Helper()
	leaseTTL := cfg.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = defaultLeaseTTL
	}
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, context.Context, NewStore, string, time.Duration)
	}{
		{"PutIfAbsent", testPutIfAbsent},
		{"PutIfAbsentOnPermanentKey", testPutIfAbsentOnPermanentKey},
		{"PutIfAbsentAfterLeaseExpires", testPutIfAbsentAfterLeaseExpires},
		{"CompareAndSwap", testCompareAndSwap},
		{"CompareAndSwapOnExpiredLease", testCompareAndSwapOnExpiredLease},
		{"GetHidesExpiredLease", testGetHidesExpiredLease},
		{"ConditionalDelete", testConditionalDelete},
		{"ConditionalDeleteIgnoresExpiry", testConditionalDeleteIgnoresExpiry},
		{"UnconditionalDeleteIsIdempotent", testUnconditionalDeleteIsIdempotent},
		{"RenewRequiresOwnership", testRenewRequiresOwnership},
		{"RenewOnPermanentKey", testRenewOnPermanentKey},
		{"RenewRejectsNonPositiveTTL", testRenewRejectsNonPositiveTTL},
		{"ExactlyOneWinsTheSeedRace", testExactlyOneWinsTheSeedRace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, context.Background(), newStore, cfg.KeyPrefix+"/"+tc.name, leaseTTL)
		})
	}
}

func mustPut(t *testing.T, ctx context.Context, kv provider.KVStore, key, val string, ttl time.Duration) {
	t.Helper()
	ok, err := kv.PutIfAbsent(ctx, key, val, ttl)
	if err != nil || !ok {
		t.Fatalf("setup PutIfAbsent(%q): ok=%v err=%v", key, ok, err)
	}
}

func testPutIfAbsent(t *testing.T, ctx context.Context, newStore NewStore, key string, _ time.Duration) {
	kv := newStore("owner")
	mustPut(t, ctx, kv, key, "v1", 0)

	ok, err := kv.PutIfAbsent(ctx, key, "v2", 0)
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if ok {
		t.Fatal("a live key must not be overwritten")
	}
	if v, present, _ := kv.Get(ctx, key); !present || v != "v1" {
		t.Fatalf("get: v=%q present=%v, want the first value", v, present)
	}
}

// A permanent key has no lease to lapse, so it is never up for grabs. This is
// the difference between "the holder died" and "someone deliberately claimed
// this forever", and getting it wrong hands a live key to a second writer.
func testPutIfAbsentOnPermanentKey(t *testing.T, ctx context.Context, newStore NewStore, key string, leaseTTL time.Duration) {
	kv := newStore("owner")
	mustPut(t, ctx, kv, key, "permanent", 0)

	time.Sleep(leaseTTL)
	ok, err := kv.PutIfAbsent(ctx, key, "usurper", 0)
	if err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}
	if ok {
		t.Fatal("a key with no lease must never be claimable")
	}
}

// The lease expiring is what lets the cluster recover when the holder dies
// before it can finish bootstrapping.
func testPutIfAbsentAfterLeaseExpires(t *testing.T, ctx context.Context, newStore NewStore, key string, leaseTTL time.Duration) {
	kv := newStore("owner")
	mustPut(t, ctx, kv, key, "held", leaseTTL)

	if ok, _ := kv.PutIfAbsent(ctx, key, "too-early", leaseTTL); ok {
		t.Fatal("a live lease must not be claimable")
	}
	time.Sleep(leaseTTL * 2)

	ok, err := kv.PutIfAbsent(ctx, key, "claimed", leaseTTL)
	if err != nil || !ok {
		t.Fatalf("after expiry: ok=%v err=%v", ok, err)
	}
	if v, _, _ := kv.Get(ctx, key); v != "claimed" {
		t.Fatalf("value after reclaim = %q", v)
	}
}

func testCompareAndSwap(t *testing.T, ctx context.Context, newStore NewStore, key string, _ time.Duration) {
	kv := newStore("owner")
	mustPut(t, ctx, kv, key, "a", 0)

	if ok, err := kv.CompareAndSwap(ctx, key, "wrong", "b", 0); err != nil || ok {
		t.Fatalf("CAS with a stale old value: ok=%v err=%v", ok, err)
	}
	if ok, err := kv.CompareAndSwap(ctx, key, "a", "b", 0); err != nil || !ok {
		t.Fatalf("CAS with the current value: ok=%v err=%v", ok, err)
	}
	if v, _, _ := kv.Get(ctx, key); v != "b" {
		t.Fatalf("after CAS v=%q", v)
	}
	if ok, _ := kv.CompareAndSwap(ctx, "missing-"+key, "a", "b", 0); ok {
		t.Fatal("CAS on an absent key must fail")
	}
}

// An expired lease is gone as far as readers are concerned, so a CAS against
// the value it used to hold must not succeed.
func testCompareAndSwapOnExpiredLease(t *testing.T, ctx context.Context, newStore NewStore, key string, leaseTTL time.Duration) {
	kv := newStore("owner")
	mustPut(t, ctx, kv, key, "a", leaseTTL)
	time.Sleep(leaseTTL * 2)

	if ok, err := kv.CompareAndSwap(ctx, key, "a", "b", 0); err != nil || ok {
		t.Fatalf("CAS against an expired lease: ok=%v err=%v", ok, err)
	}
}

func testGetHidesExpiredLease(t *testing.T, ctx context.Context, newStore NewStore, key string, leaseTTL time.Duration) {
	kv := newStore("owner")
	mustPut(t, ctx, kv, key, "x", leaseTTL)
	if _, present, err := kv.Get(ctx, key); err != nil || !present {
		t.Fatalf("live lease: present=%v err=%v", present, err)
	}
	time.Sleep(leaseTTL * 2)

	// Both DynamoDB TTL and GCS lifecycle rules delete lazily -- hours or days
	// late -- so every implementation has to filter expiry on read rather than
	// trust the backend to have reaped the item.
	if v, present, err := kv.Get(ctx, key); err != nil || present {
		t.Fatalf("expired lease: v=%q present=%v err=%v, want absent", v, present, err)
	}
	if _, present, err := kv.Get(ctx, "never-written-"+key); err != nil || present {
		t.Fatalf("absent key: present=%v err=%v", present, err)
	}
}

func testConditionalDelete(t *testing.T, ctx context.Context, newStore NewStore, key string, _ time.Duration) {
	kv := newStore("owner")
	mustPut(t, ctx, kv, key, "a", 0)

	wrong := "b"
	if ok, err := kv.Delete(ctx, key, &wrong); err != nil || ok {
		t.Fatalf("delete with a non-matching value: ok=%v err=%v", ok, err)
	}
	right := "a"
	if ok, err := kv.Delete(ctx, key, &right); err != nil || !ok {
		t.Fatalf("delete with the matching value: ok=%v err=%v", ok, err)
	}
	if _, present, _ := kv.Get(ctx, key); present {
		t.Fatal("key should be gone")
	}
}

// A conditional delete compares the raw value and ignores expiry. A script
// releasing its own lease during shutdown depends on this: the lease may well
// have lapsed while the process was stopping, and the cleanup still has to
// happen.
func testConditionalDeleteIgnoresExpiry(t *testing.T, ctx context.Context, newStore NewStore, key string, leaseTTL time.Duration) {
	kv := newStore("owner")
	mustPut(t, ctx, kv, key, "mine", leaseTTL)
	time.Sleep(leaseTTL * 2)

	mine := "mine"
	if ok, err := kv.Delete(ctx, key, &mine); err != nil || !ok {
		t.Fatalf("delete of an expired but matching entry: ok=%v err=%v", ok, err)
	}
}

// Deleting something that is not there leaves the caller in the state it asked
// for, so it reports success.
func testUnconditionalDeleteIsIdempotent(t *testing.T, ctx context.Context, newStore NewStore, key string, _ time.Duration) {
	kv := newStore("owner")
	if ok, err := kv.Delete(ctx, key, nil); err != nil || !ok {
		t.Fatalf("delete of an absent key: ok=%v err=%v", ok, err)
	}
	mustPut(t, ctx, kv, key, "v", 0)
	if ok, err := kv.Delete(ctx, key, nil); err != nil || !ok {
		t.Fatalf("delete of a present key: ok=%v err=%v", ok, err)
	}
	if ok, err := kv.Delete(ctx, key, nil); err != nil || !ok {
		t.Fatalf("second delete: ok=%v err=%v", ok, err)
	}
}

// The owner check is what stops a replacement instance from renewing a lease it
// never held -- which matters most on platforms where the replacement inherits
// its predecessor's name.
func testRenewRequiresOwnership(t *testing.T, ctx context.Context, newStore NewStore, key string, leaseTTL time.Duration) {
	a, b := newStore("owner-a"), newStore("owner-b")
	mustPut(t, ctx, a, key, "held", leaseTTL)

	if ok, err := a.Renew(ctx, key, time.Minute); err != nil || !ok {
		t.Fatalf("owner renewing its own live lease: ok=%v err=%v", ok, err)
	}
	if ok, err := b.Renew(ctx, key, time.Minute); err != nil || ok {
		t.Fatalf("non-owner renewing: ok=%v err=%v, want refused", ok, err)
	}
	if ok, err := a.Renew(ctx, "absent-"+key, time.Minute); err != nil || ok {
		t.Fatalf("renewing an absent key: ok=%v err=%v", ok, err)
	}
}

// A permanent key carries no lease, so there is nothing to extend.
func testRenewOnPermanentKey(t *testing.T, ctx context.Context, newStore NewStore, key string, leaseTTL time.Duration) {
	kv := newStore("owner")
	mustPut(t, ctx, kv, key, "permanent", 0)

	if ok, err := kv.Renew(ctx, key, time.Minute); err != nil || ok {
		t.Fatalf("renew on a key with no lease: ok=%v err=%v, want refused", ok, err)
	}
}

func testRenewRejectsNonPositiveTTL(t *testing.T, ctx context.Context, newStore NewStore, key string, leaseTTL time.Duration) {
	kv := newStore("owner")
	mustPut(t, ctx, kv, key, "held", leaseTTL)

	// Renewing "forever" would turn a lease into a permanent key and leave the
	// lock unreleasable if the holder then died.
	if _, err := kv.Renew(ctx, key, 0); err == nil {
		t.Error("renew with a zero ttl should error")
	}
	if _, err := kv.Renew(ctx, key, -time.Second); err == nil {
		t.Error("renew with a negative ttl should error")
	}
}

// The reason this store exists. Every replica of a cold-starting cluster races
// for one lease at the same instant, and exactly one has to win -- a store whose
// put-if-absent is not genuinely atomic lets two nodes each bootstrap their own
// cluster, which is the split brain muster is there to prevent.
func testExactlyOneWinsTheSeedRace(t *testing.T, ctx context.Context, newStore NewStore, key string, _ time.Duration) {
	const racers = 12

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		mu    sync.Mutex
		wins  []string
		errs  []error
	)
	for i := range racers {
		owner := fmt.Sprintf("owner-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			kv := newStore(owner)
			<-start // let them collide rather than queue
			ok, err := kv.PutIfAbsent(ctx, key, owner, time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else if ok {
				wins = append(wins, owner)
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("PutIfAbsent: %v", err)
	}
	if len(wins) != 1 {
		t.Fatalf("%d racers, %d winners (%s), want exactly 1", racers, len(wins), strings.Join(wins, ", "))
	}
	// The winner's value is what the losers will read and follow, so it has to
	// be the one that actually won.
	if v, present, err := newStore("reader").Get(ctx, key); err != nil || !present || v != wins[0] {
		t.Fatalf("stored value = %q present=%v err=%v, want %q", v, present, err, wins[0])
	}
}
