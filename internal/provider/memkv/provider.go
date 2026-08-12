package memkv

import (
	"context"
	"os"

	"github.com/moriyoshi/muster/internal/provider"
)

// Name is the registered provider name.
const Name = "mem"

// Factory builds the in-process provider: a kv store and nothing else. It exists
// so muster can be run and a script exercised with no cloud account at all --
// seed election against a single-process store still behaves, which is most of
// what a script author needs to iterate on.
//
// It is never autodetected; you have to ask for it by name.
type Factory struct{}

var _ provider.Factory = Factory{}

func (Factory) Name() string { return Name }

func (Factory) Options() []provider.OptionSpec { return nil }

func (Factory) Open(_ context.Context, cfg provider.Config) (provider.Provider, error) {
	if err := provider.ValidateOptions(cfg.Options, Factory{}.Options()); err != nil {
		return nil, err
	}
	owner, _ := os.Hostname()
	return &memProvider{
		Unimplemented: provider.Unimplemented{ProviderName: Name},
		kv:            New(owner),
		enabled:       cfg.KVStore != "",
	}, nil
}

type memProvider struct {
	provider.Unimplemented
	kv      *KV
	enabled bool
}

var _ provider.Provider = (*memProvider)(nil)

// KV is gated on -kv-store like every other provider's, so a script that forgets
// to enable it fails the same way here as it would in production rather than
// silently working in development.
func (p *memProvider) KV(context.Context) (provider.KVStore, error) {
	if !p.enabled {
		return nil, provider.ErrNotConfigured
	}
	return p.kv, nil
}

func init() { provider.Register(Factory{}) }
