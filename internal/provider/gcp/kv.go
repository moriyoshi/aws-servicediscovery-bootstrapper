//go:build gcp

package gcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	"github.com/moriyoshi/muster/internal/provider"
)

// leasePrefix keeps every key muster writes under one path, so the bucket's
// lifecycle rule can reap stale objects without touching anything else that may
// share the bucket.
const leasePrefix = "leases"

// gcsKV is the Cloud Storage-backed provider.KVStore. One object per key: the
// value is the object body and the lease lives in custom metadata.
//
// Atomicity comes from generation preconditions. ifGenerationMatch=0 writes only
// when the object does not exist; ifGenerationMatch=G writes only while it is
// still at the generation we read. That is strictly stronger than the DynamoDB
// store's conditions, whose compare-and-swap tests `val = :old` and so can be
// fooled by an A->B->A sequence between a script's read and its write.
// Generations are monotonic per object, so nothing can be mistaken for the
// version it replaced.
//
// GCS has no usable TTL -- lifecycle rules are age-in-days and run about once a
// day -- so expiry is filtered application-side. That is not a workaround for
// this backend in particular: DynamoDB's TTL deletion is lazy too, and its
// store filters expiry on read for the same reason.
//
// Object metadata: owner (S), ttl_ms (N, absent means permanent), expires_at
// (RFC 3339, informational only -- see read for what actually decides).
//
// The lease length is stored in milliseconds, not seconds. DynamoDB has to use
// whole seconds because expires_at is its TTL attribute and the service defines
// it that way; here the metadata is ours, and truncating a sub-second lease to
// zero would read back as "no lease at all" -- a permanent key nobody can ever
// reclaim.
type gcsKV struct {
	client *storage.Client
	bucket string
	prefix string
	owner  string

	// project and location are only needed by Provision, which creates the
	// bucket; ordinary operation addresses the bucket by name alone.
	project  string
	location string

	now func() time.Time // injectable for tests
}

var (
	_ provider.KVStore     = (*gcsKV)(nil)
	_ provider.Provisioner = (*gcsKV)(nil)
)

func newGCSKV(client *storage.Client, bucket, prefix, owner, project, location string) *gcsKV {
	return &gcsKV{
		client: client, bucket: bucket, prefix: prefix, owner: owner,
		project: project, location: location, now: time.Now,
	}
}

func (g *gcsKV) obj(key string) *storage.ObjectHandle {
	name := leasePrefix + "/"
	if g.prefix != "" {
		name += g.prefix + "/"
	}
	return g.client.Bucket(g.bucket).Object(name + key)
}

// isPrecondFailed reports whether err is a 412 from a failed generation
// precondition -- the analogue of DynamoDB's ConditionalCheckFailedException,
// and like it a negative result rather than a failure.
func isPrecondFailed(err error) bool {
	var ae *googleapi.Error
	return errors.As(err, &ae) && ae.Code == 412
}

func isNotExist(err error) bool {
	if errors.Is(err, storage.ErrObjectNotExist) {
		return true
	}
	var ae *googleapi.Error
	return errors.As(err, &ae) && ae.Code == 404
}

// entryState is one snapshot of a key: its value, the generation it was read at
// (the anchor every conditional write uses), and its lease.
type entryState struct {
	val     string
	gen     int64
	owner   string
	expires time.Time // zero means permanent
}

func (e entryState) live(now time.Time) bool {
	return e.expires.IsZero() || e.expires.After(now)
}

// read fetches value, generation and lease.
//
// It is deliberately two calls rather than one. A Reader exposes custom
// metadata only as it came off the wire -- x-goog-meta-* headers, whose names
// Go canonicalises, so "ttl_ms" reads back as "Ttl_ms" -- and its LastModified
// comes from the Last-Modified header, which carries whole seconds. Both are
// silent: the lease length parses as absent, which reads as a permanent key
// nobody can reclaim. ObjectAttrs gives the metadata under the names we wrote
// and a full-precision Updated. Pinning the body read to the generation Attrs
// returned keeps the pair consistent even if the object changes in between.
//
// Expiry is not applied here: Delete with an ifValue has to match expired
// entries too, so each caller decides. Callers that need liveness use
// entryState.live.
func (g *gcsKV) read(ctx context.Context, key string) (entryState, bool, error) {
	obj := g.obj(key)
	attrs, err := obj.Attrs(ctx)
	if isNotExist(err) {
		return entryState{}, false, nil
	}
	if err != nil {
		return entryState{}, false, err
	}

	r, err := obj.Generation(attrs.Generation).NewReader(ctx)
	if isNotExist(err) {
		return entryState{}, false, nil // deleted between the two calls
	}
	if err != nil {
		return entryState{}, false, err
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		return entryState{}, false, err
	}

	e := entryState{val: string(body), gen: attrs.Generation, owner: attrs.Metadata["owner"]}
	if ms, perr := strconv.ParseInt(attrs.Metadata["ttl_ms"], 10, 64); perr == nil && ms > 0 {
		// Anchored on the server's write timestamp plus the lease length, not
		// on an absolute the writer computed. The DynamoDB store compares a
		// deadline from the writer's clock against the reader's, so skew
		// between two nodes shifts when a lease looks expired; here the writer's
		// clock does not enter into it.
		e.expires = attrs.Updated.Add(time.Duration(ms) * time.Millisecond)
	}
	return e, true, nil
}

// write puts val under cond, which must always carry a precondition: a
// preconditioned write is idempotent, so the storage client retries it safely,
// and an unconditioned write here would be a bug rather than an optimisation.
func (g *gcsKV) write(ctx context.Context, key, val string, ttl time.Duration, cond storage.Conditions) (bool, error) {
	md := map[string]string{"owner": g.owner}
	if ttl > 0 {
		md["ttl_ms"] = strconv.FormatInt(ttl.Milliseconds(), 10)
		// For a human reading the bucket. Not authoritative: read() recomputes
		// expiry from the server's write time, so this is the only place the
		// writer's clock appears at all.
		md["expires_at"] = g.now().Add(ttl).UTC().Format(time.RFC3339)
	}
	w := g.obj(key).If(cond).NewWriter(ctx)
	w.ContentType = "text/plain"
	w.Metadata = md
	if _, err := io.WriteString(w, val); err != nil {
		_ = w.Close()
		return false, err
	}
	// The precondition is enforced at Close, where the request is actually
	// issued, so this is the error that matters.
	if err := w.Close(); err != nil {
		if isPrecondFailed(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (g *gcsKV) PutIfAbsent(ctx context.Context, key, val string, ttl time.Duration) (bool, error) {
	// Fast path: nothing there at all.
	ok, err := g.write(ctx, key, val, ttl, storage.Conditions{DoesNotExist: true})
	if err != nil {
		return false, fmt.Errorf("kv put_if_absent %q: %w", key, err)
	}
	if ok {
		return true, nil
	}

	// Something is there. DynamoDB expresses "absent or expired" in one
	// condition because expiry is a stored attribute it can compare; here it is
	// application-level, so this is a read followed by a write anchored on the
	// generation that read observed. Two nodes that both see the lease expired
	// still contend on that generation, so exactly one wins.
	e, found, err := g.read(ctx, key)
	if err != nil {
		return false, fmt.Errorf("kv put_if_absent %q: %w", key, err)
	}
	if !found {
		// Deleted between our write and our read. Returning false is safe --
		// callers poll -- and retrying here would be an unbounded livelock
		// under contention.
		return false, nil
	}
	if e.live(g.now()) {
		return false, nil // someone holds it; the common "we lost the race" case
	}
	ok, err = g.write(ctx, key, val, ttl, storage.Conditions{GenerationMatch: e.gen})
	if err != nil {
		return false, fmt.Errorf("kv put_if_absent %q: %w", key, err)
	}
	return ok, nil
}

func (g *gcsKV) CompareAndSwap(ctx context.Context, key, oldV, newV string, ttl time.Duration) (bool, error) {
	e, found, err := g.read(ctx, key)
	if err != nil {
		return false, fmt.Errorf("kv compare_and_swap %q: %w", key, err)
	}
	// The current value must match and still be live, matching the DynamoDB
	// condition `val = :old AND (attribute_not_exists(expires_at) OR expires_at
	// > :now)`.
	if !found || !e.live(g.now()) || e.val != oldV {
		return false, nil
	}
	// Conditioning on the generation we read the value at is what makes this
	// immune to A->B->A.
	ok, err := g.write(ctx, key, newV, ttl, storage.Conditions{GenerationMatch: e.gen})
	if err != nil {
		return false, fmt.Errorf("kv compare_and_swap %q: %w", key, err)
	}
	return ok, nil
}

// Get is strongly consistent without asking: GCS object reads have been
// globally strongly consistent since 2020, so there is no ConsistentRead to
// set.
func (g *gcsKV) Get(ctx context.Context, key string) (string, bool, error) {
	e, found, err := g.read(ctx, key)
	if err != nil {
		return "", false, fmt.Errorf("kv get %q: %w", key, err)
	}
	if !found || !e.live(g.now()) {
		return "", false, nil // absent, or expired and not yet reaped
	}
	return e.val, true, nil
}

func (g *gcsKV) Delete(ctx context.Context, key string, ifValue *string) (bool, error) {
	if ifValue == nil {
		// Reports success when there was nothing to delete: the caller got the
		// state it asked for, and both other stores agree.
		err := g.obj(key).Delete(ctx)
		if err == nil || isNotExist(err) {
			return true, nil
		}
		return false, fmt.Errorf("kv delete %q: %w", key, err)
	}

	e, found, err := g.read(ctx, key)
	if err != nil {
		return false, fmt.Errorf("kv delete %q: %w", key, err)
	}
	// No liveness check on purpose. A conditional delete compares the raw value
	// and ignores expiry, because a script releasing its own lease during
	// shutdown still has to clean up if the lease lapsed while it was stopping.
	if !found || e.val != *ifValue {
		return false, nil
	}
	err = g.obj(key).If(storage.Conditions{GenerationMatch: e.gen}).Delete(ctx)
	if err != nil {
		if isPrecondFailed(err) || isNotExist(err) {
			return false, nil // changed or removed under us
		}
		return false, fmt.Errorf("kv delete %q: %w", key, err)
	}
	return true, nil
}

func (g *gcsKV) Renew(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("kv renew %q: ttl must be positive", key)
	}
	e, found, err := g.read(ctx, key)
	if err != nil {
		return false, fmt.Errorf("kv renew %q: %w", key, err)
	}
	// A permanent key has no lease to extend, and renewing one would quietly
	// turn it into a lease that can then expire. The owner check is what stops a
	// replacement instance from renewing a lease it never held -- which is why
	// the owner carries the numeric instance id and not just the name.
	if !found || e.expires.IsZero() || !e.live(g.now()) || e.owner != g.owner {
		return false, nil
	}
	// Rewriting the same body under GenerationMatch is the read-modify-write
	// equivalent of DynamoDB's single conditional UpdateItem, and it refreshes
	// the server timestamp the new lease is anchored on.
	ok, err := g.write(ctx, key, e.val, ttl, storage.Conditions{GenerationMatch: e.gen})
	if err != nil {
		return false, fmt.Errorf("kv renew %q: %w", key, err)
	}
	return ok, nil
}

// Provision creates the bucket behind -kv-create.
func (g *gcsKV) Provision(ctx context.Context) error {
	if g.project == "" {
		return errors.New("-kv-create needs a project; pass -provider-opt project=<id>")
	}
	b := g.client.Bucket(g.bucket)
	if _, err := b.Attrs(ctx); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrBucketNotExist) {
		return fmt.Errorf("describe kv bucket: %w", err)
	}
	err := b.Create(ctx, g.project, &storage.BucketAttrs{
		Location:                 g.location,
		StorageClass:             "STANDARD",
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
		// Soft delete is on by default (7 days, and billed) for buckets created
		// since 2024. A lease object is rewritten on every renew, so it would
		// accumulate -- and charge for -- a superseded generation each time.
		SoftDeletePolicy: &storage.SoftDeletePolicy{RetentionDuration: 0},
		// A janitor, not the lease timer: lifecycle age is in days and runs
		// about daily. Expiry is enforced on read. Scoped to the lease prefix so
		// it cannot reap anything else in the bucket.
		Lifecycle: storage.Lifecycle{Rules: []storage.LifecycleRule{{
			Action:    storage.LifecycleAction{Type: storage.DeleteAction},
			Condition: storage.LifecycleCondition{AgeInDays: 1, MatchesPrefix: []string{leasePrefix + "/"}},
		}}},
	})
	if err != nil {
		return fmt.Errorf("create kv bucket: %w", err)
	}
	return nil
}
