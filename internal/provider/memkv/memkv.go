package memkv

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/moriyoshi/muster/internal/provider"
)

// KV is an in-memory provider.KVStore for offline use: unit tests, and the
// `mem` provider that lets muster run with no cloud at all. It reproduces the
// DynamoDB store's semantics (TTL leases, owner-scoped renew, conditional
// writes, and the four edge cases documented on provider.KVStore) and is safe
// for concurrent use, so several engines can race one shared store.
type KV struct {
	// mu and data are pointers/maps rather than values so that WithOwner can
	// hand out a second owner over the same store, which is the only way lease
	// ownership can be exercised at all.
	mu    *sync.Mutex
	owner string
	data  map[string]entry
}

type entry struct {
	val     string
	owner   string
	expires time.Time // zero means no expiry
}

var _ provider.KVStore = (*KV)(nil)

func New(owner string) *KV {
	return &KV{mu: new(sync.Mutex), owner: owner, data: map[string]entry{}}
}

// WithOwner returns a view of the same store under a different owner, the way a
// second instance would see one shared backing store. Lease ownership is only
// meaningful between two owners, so there is no way to exercise it otherwise.
func (m *KV) WithOwner(owner string) *KV {
	return &KV{mu: m.mu, owner: owner, data: m.data}
}

// live returns the entry for k only if it exists and its lease has not expired.
func (m *KV) live(k string, now time.Time) (entry, bool) {
	e, ok := m.data[k]
	if !ok {
		return entry{}, false
	}
	if !e.expires.IsZero() && !e.expires.After(now) {
		return entry{}, false
	}
	return e, true
}

func expiryAt(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

func (m *KV) PutIfAbsent(_ context.Context, key, val string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if _, ok := m.live(key, now); ok {
		return false, nil
	}
	m.data[key] = entry{val: val, owner: m.owner, expires: expiryAt(now, ttl)}
	return true, nil
}

func (m *KV) CompareAndSwap(_ context.Context, key, oldV, newV string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	e, ok := m.live(key, now)
	if !ok || e.val != oldV {
		return false, nil
	}
	m.data[key] = entry{val: newV, owner: m.owner, expires: expiryAt(now, ttl)}
	return true, nil
}

func (m *KV) Get(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live(key, time.Now())
	if !ok {
		return "", false, nil
	}
	return e.val, true, nil
}

func (m *KV) Delete(_ context.Context, key string, ifValue *string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ifValue != nil {
		e, ok := m.data[key]
		if !ok || e.val != *ifValue {
			return false, nil
		}
	}
	delete(m.data, key)
	return true, nil
}

func (m *KV) Renew(_ context.Context, key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("kv renew %q: ttl must be positive", key)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	e, ok := m.live(key, now)
	// A permanent key carries no lease to extend, and renewing one would
	// quietly convert it into a lease that can then expire. dynamoKV refuses it
	// because its condition is expires_at > :now, which an absent attribute
	// never satisfies; this matches deliberately.
	if !ok || e.expires.IsZero() || e.owner != m.owner {
		return false, nil
	}
	e.expires = expiryAt(now, ttl)
	m.data[key] = e
	return true, nil
}
