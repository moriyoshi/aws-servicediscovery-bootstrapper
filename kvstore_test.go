package main

import (
	"context"
	"testing"
	"time"
)

func TestMemKVPutIfAbsent(t *testing.T) {
	ctx := context.Background()
	kv := newMemKV("owner")

	ok, err := kv.PutIfAbsent(ctx, "k", "v1", 0)
	if err != nil || !ok {
		t.Fatalf("first put: ok=%v err=%v", ok, err)
	}
	ok, _ = kv.PutIfAbsent(ctx, "k", "v2", 0)
	if ok {
		t.Fatal("second put should fail (key present)")
	}
	if v, present, _ := kv.Get(ctx, "k"); !present || v != "v1" {
		t.Fatalf("get: v=%q present=%v", v, present)
	}
}

func TestMemKVExpiry(t *testing.T) {
	ctx := context.Background()
	kv := newMemKV("owner")
	kv.PutIfAbsent(ctx, "lease", "x", 50*time.Millisecond)
	if _, present, _ := kv.Get(ctx, "lease"); !present {
		t.Fatal("lease should be live")
	}
	time.Sleep(80 * time.Millisecond)
	if _, present, _ := kv.Get(ctx, "lease"); present {
		t.Fatal("lease should have expired")
	}
	// after expiry, put_if_absent succeeds again
	if ok, _ := kv.PutIfAbsent(ctx, "lease", "y", 0); !ok {
		t.Fatal("put after expiry should succeed")
	}
}

func TestMemKVCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	kv := newMemKV("owner")
	kv.PutIfAbsent(ctx, "k", "a", 0)
	if ok, _ := kv.CompareAndSwap(ctx, "k", "wrong", "b", 0); ok {
		t.Fatal("CAS with wrong old should fail")
	}
	if ok, _ := kv.CompareAndSwap(ctx, "k", "a", "b", 0); !ok {
		t.Fatal("CAS with right old should succeed")
	}
	if v, _, _ := kv.Get(ctx, "k"); v != "b" {
		t.Fatalf("after CAS v=%q", v)
	}
}

func TestMemKVDeleteConditional(t *testing.T) {
	ctx := context.Background()
	kv := newMemKV("owner")
	kv.PutIfAbsent(ctx, "k", "a", 0)
	wrong := "b"
	if ok, _ := kv.Delete(ctx, "k", &wrong); ok {
		t.Fatal("conditional delete with wrong value should not delete")
	}
	right := "a"
	if ok, _ := kv.Delete(ctx, "k", &right); !ok {
		t.Fatal("conditional delete with right value should delete")
	}
	if _, present, _ := kv.Get(ctx, "k"); present {
		t.Fatal("key should be gone")
	}
}

func TestMemKVRenewOwnership(t *testing.T) {
	ctx := context.Background()
	kv := newMemKV("owner-a")
	kv.PutIfAbsent(ctx, "lease", "x", 30*time.Millisecond)
	if ok, _ := kv.Renew(ctx, "lease", time.Minute); !ok {
		t.Fatal("owner should be able to renew its own live lease")
	}
	// a different owner cannot renew
	other := newMemKV("owner-b")
	other.data = kv.data // share the backing map
	if ok, _ := other.Renew(ctx, "lease", time.Minute); ok {
		t.Fatal("non-owner should not renew")
	}
	if _, err := kv.Renew(ctx, "lease", 0); err == nil {
		t.Fatal("renew with non-positive ttl should error")
	}
}
