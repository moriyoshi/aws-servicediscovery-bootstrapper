//go:build gcp

package gcp

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/firestore"

	"github.com/moriyoshi/muster/internal/provider"
	"github.com/moriyoshi/muster/internal/provider/kvtest"
)

func TestDocID(t *testing.T) {
	// A key routinely contains "/", which a Firestore document id may not.
	for in, want := range map[string]string{
		"tikv-pd/seed": "kv-tikv-pd%2Fseed",
		"plain":        "kv-plain",
		"100%":         "kv-100%25",
		// Escaping percent as well is what keeps the mapping injective: without
		// it, "a%2Fb" and "a/b" would collide on one document.
		"a%2Fb": "kv-a%252Fb",
		"":      "kv-",
	} {
		if got := docID(in); got != want {
			t.Errorf("docID(%q) = %q, want %q", in, got, want)
		}
	}

	// Firestore reserves "." and "..", and anything matching __.*__. The prefix
	// is what keeps every id clear of all three.
	for _, in := range []string{".", "..", "__proto__"} {
		id := docID(in)
		if id == "." || id == ".." {
			t.Errorf("docID(%q) = %q, which Firestore reserves", in, id)
		}
		if len(id) >= 4 && id[:2] == "__" {
			t.Errorf("docID(%q) = %q, which matches Firestore's reserved __.*__", in, id)
		}
	}
}

// TestFirestoreConformance runs the shared suite against a Firestore emulator.
//
// It is skipped unless FIRESTORE_EMULATOR_HOST is set, because there is no
// in-process Firestore fake: the emulator is a separate process, written in
// Java. Docker is the way to run it without putting a JRE on the host --
//
//	docker run -d --name muster-fs-emu -p 8484:8484 \
//	  gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators \
//	  gcloud emulators firestore start --host-port=0.0.0.0:8484
//	FIRESTORE_EMULATOR_HOST=127.0.0.1:8484 go test -tags=gcp ./internal/provider/gcp/
//
// -- or `gcloud components install cloud-firestore-emulator` with a JRE already
// present.
//
// The Cloud Storage store runs the same suite against an in-process fake in the
// ordinary test run, so the two are held to one specification either way.
//
// An emulator is not production, and the operation this leaves least proven is
// contention: ExactlyOneWinsTheSeedRace passing here says the transactions
// serialize in the emulator's implementation of them. That is far short of a
// guarantee and far past reading the code, which is the standard a mocked
// client could never clear -- a fake programmed from the same assumptions as
// the code cannot falsify them.
func TestFirestoreConformance(t *testing.T) {
	host := os.Getenv("FIRESTORE_EMULATOR_HOST")
	if host == "" {
		t.Skip("set FIRESTORE_EMULATOR_HOST to run the Firestore conformance suite")
	}
	ctx := context.Background()
	client, err := firestore.NewClientWithDatabase(ctx, "muster-test", firestore.DefaultDatabaseID)
	if err != nil {
		t.Fatalf("firestore client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// Namespaced per run: the emulator keeps state for its lifetime, and a
	// leftover permanent key would fail the next run's put-if-absent.
	collection := fmt.Sprintf("conf-%d", time.Now().UnixNano())
	kvtest.Run(t, kvtest.Config{KeyPrefix: "conf"}, func(owner string) provider.KVStore {
		return newFirestoreKV(client, collection, "", owner)
	})
}
