//go:build e2e

package aws

// End-to-end tests that exercise the DynamoDB-backed kv store
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

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/moriyoshi/muster/internal/provider"
	"github.com/moriyoshi/muster/internal/provider/kvtest"
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

func e2eKV(t *testing.T, endpoint, owner string) *DynamoKV {
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
	return NewDynamoKV(client, "wtb-e2e-kv", "", owner)
}

// TestE2EDynamoKVConformance runs the shared kvStore conformance suite against
// the emulator. The in-memory store runs the same suite in the ordinary test
// run, so any divergence between the two shows up as a named subtest rather
// than as a cluster that split in production.
func TestE2EDynamoKVConformance(t *testing.T) {
	endpoint := e2eEndpoint(t)
	if err := e2eKV(t, endpoint, "setup").Provision(context.Background()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Keys are namespaced per run: the emulator keeps state between runs, and a
	// leftover permanent key would fail the next run's put-if-absent.
	kvtest.Run(t, kvtest.Config{
		KeyPrefix: fmt.Sprintf("conf-%d", time.Now().UnixNano()),
		// DynamoDB stores expires_at in whole seconds, so anything sub-second
		// rounds to "already expired" and the expiry cases would assert nothing.
		LeaseTTL: 3 * time.Second,
	}, func(owner string) provider.KVStore {
		return e2eKV(t, endpoint, owner)
	})
}
