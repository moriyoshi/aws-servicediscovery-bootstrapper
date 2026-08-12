package provider

import (
	"context"
	"time"
)

// KVStore is a small conditional-write key/value store used by Starlark scripts
// for cluster coordination (seed election, membership registry). Implementations
// MUST provide atomic put-if-absent / compare-and-swap semantics with TTL leases,
// so that exactly one node can win a lock even under a concurrent cold start.
//
// A ttl of 0 means "no expiry" (a permanent key). A positive ttl records a lease
// that expires after the given duration; expired leases behave as if absent.
//
// Four edge cases are load-bearing and every implementation must agree on them
// (the shared conformance suite pins all four):
//
//   - PutIfAbsent on a permanent key returns false -- there is no lease to lapse.
//   - Delete with ifValue compares the raw value and ignores expiry, so it still
//     reaps an expired entry whose value matches. A script releasing its own
//     lease during shutdown depends on this.
//   - Delete without ifValue on a missing key returns true: the caller got the
//     state it asked for.
//   - Renew on a permanent key returns false -- again, no lease.
type KVStore interface {
	// PutIfAbsent writes key=val only if the key is absent or its lease has
	// expired. Returns true if written, false if a live value already exists.
	PutIfAbsent(ctx context.Context, key, val string, ttl time.Duration) (bool, error)
	// CompareAndSwap sets key=newV only if the current (live) value equals oldV.
	CompareAndSwap(ctx context.Context, key, oldV, newV string, ttl time.Duration) (bool, error)
	// Get returns the current live value and whether it is present. The read
	// must be consistent: seed election reads its own writes.
	Get(ctx context.Context, key string) (string, bool, error)
	// Delete removes key. When ifValue is non-nil the delete is conditional on
	// the current value equalling *ifValue. Returns true if a delete happened.
	Delete(ctx context.Context, key string, ifValue *string) (bool, error)
	// Renew extends the lease on key, but only if it still exists, is unexpired,
	// and is owned by this store's owner. Returns false if the lease was lost.
	Renew(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// Provisioner is the optional "create your own backing store" add-on behind
// -kv-create. It is a type assertion on the KVStore rather than a Provider
// method because only the startup wiring ever asks: a capability is a Provider
// method, a sub-feature of a capability you already hold is an assertion.
type Provisioner interface {
	Provision(ctx context.Context) error
}
