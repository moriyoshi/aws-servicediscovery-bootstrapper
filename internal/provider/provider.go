// Package provider defines the cloud-neutral seams muster's Starlark builtins
// sit on: peer discovery, a conditional-write key/value store, the identity of
// the instance muster runs on, orchestrator-level replica status, and (where
// the platform does not do it for us) self-registration into the service
// registry.
//
// Nothing in this package may import a cloud SDK. Implementations live in
// sibling packages -- aws (unconditional) and gcp (//go:build gcp) -- and the
// default build is asserted to link no Google Cloud packages at all.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// ErrUnsupported means the provider does not implement the capability at all --
// the cloud has nothing to back it with. ErrNotConfigured means it could, but
// the operator did not turn it on (an unset -kv-store, say).
//
// The distinction reaches the script: a builtin whose capability is missing
// raises an error carrying the reason, so "kv store not configured (set
// -kv-store)" and "kv store not supported by provider gcp" are told apart at the
// point of use rather than guessed at from a nil field.
var (
	ErrUnsupported   = errors.New("not supported by this provider")
	ErrNotConfigured = errors.New("not configured")
)

// Config is everything a Factory needs to build a Provider. Settings that mean
// the same thing on every cloud are named fields; anything one cloud alone
// understands arrives in Options, from repeated -provider-opt k=v.
type Config struct {
	Logger    *slog.Logger
	Namespace string            // -namespace: default discovery namespace
	KVStore   string            // -kv-store: table / bucket / collection name
	KVPrefix  string            // -kv-key-prefix
	Options   map[string]string // -provider-opt
}

// OptionSpec documents one -provider-opt key. Open rejects keys a factory does
// not declare, so a typo is a startup error rather than a setting that silently
// did nothing.
type OptionSpec struct {
	Key     string
	Default string
	Doc     string
}

// Factory builds a Provider. Exactly one Factory per cloud registers itself from
// an init() in a build-tagged package.
type Factory interface {
	Name() string
	Options() []OptionSpec
	Open(ctx context.Context, cfg Config) (Provider, error)
}

// ValidateOptions rejects -provider-opt keys a factory does not declare. Every
// Open should call it: an option that is quietly ignored is worse than one that
// fails, because the setting appears to have been applied.
func ValidateOptions(opts map[string]string, specs []OptionSpec) error {
	known := make(map[string]bool, len(specs))
	for _, s := range specs {
		known[s.Key] = true
	}
	var unknown []string
	for k := range opts {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	if len(specs) == 0 {
		return fmt.Errorf("takes no -provider-opt (got %s)", strings.Join(unknown, ", "))
	}
	accepted := make([]string, 0, len(specs))
	for _, s := range specs {
		accepted = append(accepted, s.Key)
	}
	sort.Strings(accepted)
	return fmt.Errorf("unknown -provider-opt %s (accepts %s)",
		strings.Join(unknown, ", "), strings.Join(accepted, ", "))
}

// Provider is one cloud's implementation of muster's capabilities. Each accessor
// is called once, during startup, and must be idempotent.
//
// Capabilities are accessors returning (T, error) rather than (T, bool) or
// optional interfaces reached by type assertion: acquiring one needs a context
// and can fail (creating a client, fetching metadata), and when it is absent the
// reason is the part the operator needs.
//
// Implementations embed Unimplemented and override what they actually have, so
// adding a capability later does not break every provider in the tree.
type Provider interface {
	// Name is the registered provider name, echoed in errors and logs.
	Name() string
	// Discovery resolves a service name to instance addresses (instances()).
	Discovery(ctx context.Context) (Discoverer, error)
	// KV is the conditional-write store behind the kv_* builtins.
	KV(ctx context.Context) (KVStore, error)
	// Fleet answers orchestrator-level questions about this workload's replicas
	// (all_replicas_running()).
	Fleet(ctx context.Context) (Fleet, error)
	// Registrar publishes this instance into the service registry, for
	// platforms that do not do it themselves. Platforms that register for you
	// -- ECS Service Connect -- return ErrUnsupported, so a script calling
	// register() there is told rather than silently doing nothing.
	Registrar(ctx context.Context) (Registrar, error)
	// Self describes the instance muster itself runs on. Data rather than an
	// interface: it is read once at startup and frozen into the SELF global.
	//
	// Implementations must memoize it. KV derives its lease owner from Self, so
	// a provider that resolved identity lazily after building the store would
	// hand out leases nobody can renew.
	Self(ctx context.Context) (*Identity, error)
	// Close releases whatever Open acquired.
	Close() error
}

// Unimplemented is the zero provider: every capability unsupported, Close a
// no-op. Embed it by value and set ProviderName to the registered name.
type Unimplemented struct{ ProviderName string }

func (u Unimplemented) Name() string { return u.ProviderName }

func (Unimplemented) Discovery(context.Context) (Discoverer, error) { return nil, ErrUnsupported }
func (Unimplemented) KV(context.Context) (KVStore, error)           { return nil, ErrUnsupported }
func (Unimplemented) Fleet(context.Context) (Fleet, error)          { return nil, ErrUnsupported }
func (Unimplemented) Registrar(context.Context) (Registrar, error)  { return nil, ErrUnsupported }
func (Unimplemented) Self(context.Context) (*Identity, error)       { return nil, ErrUnsupported }
func (Unimplemented) Close() error                                  { return nil }
