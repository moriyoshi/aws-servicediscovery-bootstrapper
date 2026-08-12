//go:build gcp

package gcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/moriyoshi/muster/internal/provider"
)

// firestoreKV is the Firestore-backed provider.KVStore, the alternative to the
// Cloud Storage one.
//
// Where gcsKV builds each conditional write out of a generation precondition,
// this expresses the same operations directly: a Firestore transaction reads and
// writes atomically, so put-if-absent and compare-and-swap are one round trip
// each and read exactly as the semantics are stated. That is the reason to
// choose it -- the logic is the specification rather than an encoding of it.
//
// What it costs is a database. A bucket is one idempotent call to create; a
// Firestore database is a long-running operation and a project-level decision,
// so -kv-create does not create one and this type deliberately does not
// implement provider.Provisioner. Provision it out of band.
//
// Two things are the same as the Cloud Storage store, and for the same reasons:
//
// Expiry is filtered on read. Firestore has a native TTL policy, but it deletes
// lazily -- "typically within 24 hours" -- so it is a janitor, never the lease
// timer. DynamoDB's TTL is lazy too; no backend here can delegate expiry.
//
// Expiry is anchored on the server's write time, not on a deadline the writer
// computed. A snapshot carries UpdateTime, which Firestore assigns, so a lease
// runs from when the store recorded it rather than from whenever the writer
// thought it was -- taking clock skew between nodes out of seed election.
//
// Document fields: val (string), owner (string), ttl_ms (int, absent or 0 means
// permanent), expires_at (timestamp, informational and the TTL policy's field).
type firestoreKV struct {
	client     *firestore.Client
	collection string
	prefix     string
	owner      string

	now func() time.Time // injectable for tests
}

var _ provider.KVStore = (*firestoreKV)(nil)

func newFirestoreKV(client *firestore.Client, collection, prefix, owner string) *firestoreKV {
	return &firestoreKV{
		client: client, collection: collection, prefix: prefix,
		owner: owner, now: time.Now,
	}
}

// docID turns a key into a Firestore document id.
//
// A document id may not contain "/", which muster's keys routinely do
// ("tikv-pd/seed"), so slashes are percent-escaped -- and percent itself, or
// the escaping would not be reversible. The "kv-" prefix keeps the result clear
// of the ids Firestore reserves: "." and "..", and anything matching __.*__.
func docID(key string) string {
	var b strings.Builder
	b.WriteString("kv-")
	for _, r := range key {
		switch r {
		case '/':
			b.WriteString("%2F")
		case '%':
			b.WriteString("%25")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (f *firestoreKV) doc(key string) *firestore.DocumentRef {
	if f.prefix != "" {
		key = f.prefix + "/" + key
	}
	return f.client.Collection(f.collection).Doc(docID(key))
}

// fsEntry is one snapshot of a key: the value, its lease, and the server time
// the lease runs from.
type fsEntry struct {
	val     string
	owner   string
	expires time.Time // zero means permanent
}

func (e fsEntry) live(now time.Time) bool {
	return e.expires.IsZero() || e.expires.After(now)
}

// decode reads a snapshot. found is false when the document is absent; expiry is
// not applied here, because Delete with an ifValue has to match expired entries
// too and each caller decides.
func decode(snap *firestore.DocumentSnapshot) (fsEntry, bool) {
	if snap == nil || !snap.Exists() {
		return fsEntry{}, false
	}
	var raw struct {
		Val   string `firestore:"val"`
		Owner string `firestore:"owner"`
		TTLMs int64  `firestore:"ttl_ms"`
	}
	if err := snap.DataTo(&raw); err != nil {
		return fsEntry{}, false
	}
	e := fsEntry{val: raw.Val, owner: raw.Owner}
	if raw.TTLMs > 0 {
		// UpdateTime is Firestore's, not the writer's. See the type comment.
		e.expires = snap.UpdateTime.Add(time.Duration(raw.TTLMs) * time.Millisecond)
	}
	return e, true
}

func (f *firestoreKV) payload(val string, ttl time.Duration) map[string]any {
	m := map[string]any{"val": val, "owner": f.owner}
	if ttl > 0 {
		m["ttl_ms"] = ttl.Milliseconds()
		// For a human reading the collection, and the field a TTL policy would
		// watch. Not authoritative: decode recomputes expiry from UpdateTime,
		// so this is the only place the writer's clock appears at all.
		m["expires_at"] = f.now().Add(ttl).UTC()
	}
	return m
}

// getSnap reads inside a transaction, treating "not found" as an absent
// document rather than a failure.
func txGet(tx *firestore.Transaction, ref *firestore.DocumentRef) (fsEntry, bool, error) {
	snap, err := tx.Get(ref)
	if err != nil && status.Code(err) != codes.NotFound {
		return fsEntry{}, false, err
	}
	e, found := decode(snap)
	return e, found, nil
}

func (f *firestoreKV) PutIfAbsent(ctx context.Context, key, val string, ttl time.Duration) (bool, error) {
	ref := f.doc(key)
	var won bool
	err := f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		won = false
		e, found, err := txGet(tx, ref)
		if err != nil {
			return err
		}
		// Absent, or a lease that has lapsed. A permanent key -- no ttl, so no
		// expiry -- is never claimable, which is the point of writing one.
		if found && e.live(f.now()) {
			return nil
		}
		won = true
		return tx.Set(ref, f.payload(val, ttl))
	})
	if err != nil {
		return false, fmt.Errorf("kv put_if_absent %q: %w", key, err)
	}
	return won, nil
}

func (f *firestoreKV) CompareAndSwap(ctx context.Context, key, oldV, newV string, ttl time.Duration) (bool, error) {
	ref := f.doc(key)
	var swapped bool
	err := f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		swapped = false
		e, found, err := txGet(tx, ref)
		if err != nil {
			return err
		}
		// The current value must match and still be live. Reading and writing
		// in one transaction is what makes this immune to the value being
		// changed and changed back in between.
		if !found || !e.live(f.now()) || e.val != oldV {
			return nil
		}
		swapped = true
		return tx.Set(ref, f.payload(newV, ttl))
	})
	if err != nil {
		return false, fmt.Errorf("kv compare_and_swap %q: %w", key, err)
	}
	return swapped, nil
}

// Get is strongly consistent without asking for it: Firestore document reads
// always are, so there is no ConsistentRead to set.
func (f *firestoreKV) Get(ctx context.Context, key string) (string, bool, error) {
	snap, err := f.doc(key).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("kv get %q: %w", key, err)
	}
	e, found := decode(snap)
	if !found || !e.live(f.now()) {
		return "", false, nil // absent, or expired and not yet reaped
	}
	return e.val, true, nil
}

func (f *firestoreKV) Delete(ctx context.Context, key string, ifValue *string) (bool, error) {
	ref := f.doc(key)
	if ifValue == nil {
		// Firestore's delete is idempotent, so this reports success when there
		// was nothing to delete: the caller got the state it asked for, and
		// both other stores agree.
		if _, err := ref.Delete(ctx); err != nil {
			return false, fmt.Errorf("kv delete %q: %w", key, err)
		}
		return true, nil
	}

	var deleted bool
	err := f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		deleted = false
		e, found, err := txGet(tx, ref)
		if err != nil {
			return err
		}
		// No liveness check on purpose: a conditional delete compares the raw
		// value and ignores expiry, because a script releasing its own lease
		// during shutdown still has to clean up if the lease lapsed while it
		// was stopping.
		if !found || e.val != *ifValue {
			return nil
		}
		deleted = true
		return tx.Delete(ref)
	})
	if err != nil {
		return false, fmt.Errorf("kv delete %q: %w", key, err)
	}
	return deleted, nil
}

func (f *firestoreKV) Renew(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("kv renew %q: ttl must be positive", key)
	}
	ref := f.doc(key)
	var renewed bool
	err := f.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		renewed = false
		e, found, err := txGet(tx, ref)
		if err != nil {
			return err
		}
		// A permanent key has no lease to extend, and renewing one would
		// quietly turn it into a lease that can then expire. The owner check is
		// what stops a replacement instance renewing a lease it never held.
		if !found || e.expires.IsZero() || !e.live(f.now()) || e.owner != f.owner {
			return nil
		}
		renewed = true
		// Rewriting the same value refreshes UpdateTime, which is what the new
		// lease runs from.
		return tx.Set(ref, f.payload(e.val, ttl))
	})
	if err != nil {
		return false, fmt.Errorf("kv renew %q: %w", key, err)
	}
	return renewed, nil
}
