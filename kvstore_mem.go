package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// memKV is an in-memory kvStore used for offline unit tests. It mirrors the
// dynamoKV semantics (TTL leases, owner-scoped renew, conditional writes) and is
// safe for concurrent use so several engines can race against one shared store.
type memKV struct {
	mu    sync.Mutex
	owner string
	data  map[string]memEntry
}

type memEntry struct {
	val     string
	owner   string
	expires time.Time // zero means no expiry
}

func newMemKV(owner string) *memKV {
	return &memKV{owner: owner, data: map[string]memEntry{}}
}

// live returns the entry for k only if it exists and its lease has not expired.
func (m *memKV) live(k string, now time.Time) (memEntry, bool) {
	e, ok := m.data[k]
	if !ok {
		return memEntry{}, false
	}
	if !e.expires.IsZero() && !e.expires.After(now) {
		return memEntry{}, false
	}
	return e, true
}

func expiryAt(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

func (m *memKV) PutIfAbsent(_ context.Context, key, val string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if _, ok := m.live(key, now); ok {
		return false, nil
	}
	m.data[key] = memEntry{val: val, owner: m.owner, expires: expiryAt(now, ttl)}
	return true, nil
}

func (m *memKV) CompareAndSwap(_ context.Context, key, oldV, newV string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	e, ok := m.live(key, now)
	if !ok || e.val != oldV {
		return false, nil
	}
	m.data[key] = memEntry{val: newV, owner: m.owner, expires: expiryAt(now, ttl)}
	return true, nil
}

func (m *memKV) Get(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live(key, time.Now())
	if !ok {
		return "", false, nil
	}
	return e.val, true, nil
}

func (m *memKV) Delete(_ context.Context, key string, ifValue *string) (bool, error) {
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

func (m *memKV) Renew(_ context.Context, key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("kv renew %q: ttl must be positive", key)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	e, ok := m.live(key, now)
	if !ok || e.owner != m.owner {
		return false, nil
	}
	e.expires = expiryAt(now, ttl)
	m.data[key] = e
	return true, nil
}
