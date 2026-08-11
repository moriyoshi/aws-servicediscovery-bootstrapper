//go:build e2e

// Package-level end-to-end tests that exercise the DynamoDB-backed kv store
// against a real, stateful AWS emulator — Winterbäume (github.com/moriyoshi/
// winterbaume) in its standalone winterbaume-server mode, or any AWS
// DynamoDB-compatible endpoint.
//
// Run with:
//
//	winterbaume-server &            # or any endpoint speaking the DynamoDB API
//	AWS_ENDPOINT_URL=http://127.0.0.1:8080 AWS_REGION=us-east-1 \
//	  go test -tags=e2e -run E2E ./...
//
// The endpoint is taken from AWS_ENDPOINT_URL (or WINTERBAUME_ENDPOINT). The
// test is skipped when neither is set, so it never runs in the default suite.
package main

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func e2eEndpoint(t *testing.T) string {
	t.Helper()
	for _, k := range []string{"AWS_ENDPOINT_URL", "WINTERBAUME_ENDPOINT"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	t.Skip("set AWS_ENDPOINT_URL (or WINTERBAUME_ENDPOINT) to run the e2e test")
	return ""
}

func e2eKV(t *testing.T, endpoint, owner string) *dynamoKV {
	t.Helper()
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	client := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})
	return newDynamoKV(client, "wtb-e2e-kv", "", owner)
}

// TestE2EDynamoKVSemantics mirrors the memKV parity tests against the emulator.
func TestE2EDynamoKVSemantics(t *testing.T) {
	endpoint := e2eEndpoint(t)
	ctx := context.Background()
	kv := e2eKV(t, endpoint, "owner-a")
	if err := kv.ensureTable(ctx); err != nil {
		t.Fatalf("ensureTable: %v", err)
	}

	key := "sem/" + t.Name()
	_, _ = kv.Delete(ctx, key, nil)

	if ok, err := kv.PutIfAbsent(ctx, key, "v1", 0); err != nil || !ok {
		t.Fatalf("put_if_absent: ok=%v err=%v", ok, err)
	}
	if ok, _ := kv.PutIfAbsent(ctx, key, "v2", 0); ok {
		t.Fatal("second put_if_absent should fail")
	}
	if v, present, _ := kv.Get(ctx, key); !present || v != "v1" {
		t.Fatalf("get: v=%q present=%v", v, present)
	}
	if ok, _ := kv.CompareAndSwap(ctx, key, "v1", "v2", 0); !ok {
		t.Fatal("CAS with right old should succeed")
	}
	if ok, _ := kv.CompareAndSwap(ctx, key, "v1", "v3", 0); ok {
		t.Fatal("CAS with stale old should fail")
	}
	if ok, _ := kv.Renew(ctx, key, time.Minute); !ok {
		t.Fatal("owner should renew its own lease")
	}
	other := e2eKV(t, endpoint, "owner-b")
	if ok, _ := other.Renew(ctx, key, time.Minute); ok {
		t.Fatal("non-owner should not renew")
	}
	if ok, _ := kv.Delete(ctx, key, nil); !ok {
		t.Fatal("delete should succeed")
	}
}

// TestE2ESeedElection is the split-brain safety check against the real emulator:
// N owners race a single lease; exactly one wins.
func TestE2ESeedElection(t *testing.T) {
	endpoint := e2eEndpoint(t)
	ctx := context.Background()
	setup := e2eKV(t, endpoint, "setup")
	if err := setup.ensureTable(ctx); err != nil {
		t.Fatalf("ensureTable: %v", err)
	}
	key := "seed/" + t.Name()
	_, _ = setup.Delete(ctx, key, nil)

	const n = 8
	var wg sync.WaitGroup
	wins := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kv := e2eKV(t, endpoint, "owner-"+string(rune('a'+i)))
			ok, err := kv.PutIfAbsent(ctx, key, "owner", 60*time.Second)
			if err != nil {
				t.Errorf("put_if_absent: %v", err)
			}
			wins[i] = ok
		}(i)
	}
	wg.Wait()

	won := 0
	for _, w := range wins {
		if w {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("expected exactly one seed winner, got %d", won)
	}
}
