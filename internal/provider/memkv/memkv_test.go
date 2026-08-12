package memkv

import (
	"testing"

	"github.com/moriyoshi/muster/internal/provider"
	"github.com/moriyoshi/muster/internal/provider/kvtest"
)

// The in-memory store is the reference implementation: every other backend has
// to agree with it, so it runs the shared suite first.
func TestConformance(t *testing.T) {
	shared := New("unused")
	kvtest.Run(t, kvtest.Config{KeyPrefix: "mem"}, func(owner string) provider.KVStore {
		return shared.WithOwner(owner)
	})
}
