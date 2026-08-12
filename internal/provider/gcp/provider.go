//go:build gcp

package gcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/firestore"
	servicedirectory "cloud.google.com/go/servicedirectory/apiv1"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/moriyoshi/muster/internal/provider"
)

// Name is the registered provider name.
const Name = "gcp"

// Option keys accepted via -provider-opt.
const (
	optProject    = "project"
	optLocation   = "location"
	optKVBackend  = "kv.backend"
	optKVDatabase = "kv.database"
	optEndpointS  = "endpoint.storage"
	optEndpointD  = "endpoint.servicedirectory"
)

// The stores -kv.backend selects between. Both satisfy the same conformance
// suite, so the choice is about what you are willing to provision rather than
// about semantics.
const (
	backendGCS       = "gcs"
	backendFirestore = "firestore"
)

// Factory builds the Google Cloud provider.
type Factory struct{}

var _ provider.Factory = Factory{}

func (Factory) Name() string { return Name }

func (Factory) Options() []provider.OptionSpec {
	return []provider.OptionSpec{
		{Key: optProject, Doc: "project id (default: $GOOGLE_CLOUD_PROJECT, then the metadata server)"},
		{Key: optLocation, Doc: "Service Directory location (default: this instance's region)"},
		{Key: optKVBackend, Default: backendGCS,
			Doc: "kv store backend: gcs (a bucket) or firestore (a collection)"},
		{Key: optKVDatabase, Default: "(default)",
			Doc: "Firestore database id; ignored by the gcs backend"},
		{Key: optEndpointS, Doc: "Cloud Storage endpoint override, for a fake server"},
		{Key: optEndpointD, Doc: "Service Directory endpoint override"},
	}
}

func (Factory) Open(ctx context.Context, cfg provider.Config) (provider.Provider, error) {
	if err := provider.ValidateOptions(cfg.Options, Factory{}.Options()); err != nil {
		return nil, err
	}
	if cfg.Namespace == "" {
		return nil, errors.New("namespace is required")
	}
	switch backend := cfg.Options[optKVBackend]; backend {
	case "", backendGCS, backendFirestore:
	default:
		return nil, fmt.Errorf("%s=%q: want %q or %q", optKVBackend, backend, backendGCS, backendFirestore)
	}
	p := &Provider{
		cfg:      cfg,
		logger:   cfg.Logger,
		metadata: realMetadata{c: metadata.NewClient(nil)},
	}
	return p, nil
}

// Provider is the Google Cloud implementation, targeting stateful managed
// instance groups. Each capability builds its client on first use.
type Provider struct {
	provider.Unimplemented

	cfg      provider.Config
	logger   *slog.Logger
	metadata metadataClient

	selfOnce sync.Once
	self     *provider.Identity
	selfErr  error

	mu       sync.Mutex
	sdLookup *servicedirectory.LookupClient
	sdReg    *servicedirectory.RegistrationClient
	gcs      *storage.Client
	fs       *firestore.Client
}

var _ provider.Provider = (*Provider)(nil)

func (p *Provider) Name() string { return Name }

func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var errs []error
	if p.sdLookup != nil {
		errs = append(errs, p.sdLookup.Close())
	}
	if p.sdReg != nil {
		errs = append(errs, p.sdReg.Close())
	}
	if p.gcs != nil {
		errs = append(errs, p.gcs.Close())
	}
	if p.fs != nil {
		errs = append(errs, p.fs.Close())
	}
	return errors.Join(errs...)
}

// Self is memoized because KV derives its lease owner from it. Resolving
// identity after the store was built would produce leases whose owner does not
// match, and Renew would then fail for the lifetime of the process.
func (p *Provider) Self(ctx context.Context) (*provider.Identity, error) {
	p.selfOnce.Do(func() { p.self, p.selfErr = fetchIdentity(ctx, p.metadata) })
	return p.self, p.selfErr
}

// project resolves the project id: the explicit option, then the conventional
// environment variables, then the metadata server.
func (p *Provider) project(ctx context.Context) (string, error) {
	if v := p.cfg.Options[optProject]; v != "" {
		return v, nil
	}
	for _, name := range []string{"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT", "GCP_PROJECT"} {
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
	}
	if self, err := p.Self(ctx); err == nil {
		if v := self.Extra["project"]; v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("project unknown; pass -provider-opt %s=<id> or set GOOGLE_CLOUD_PROJECT", optProject)
}

// location resolves the Service Directory location, which is a region.
func (p *Provider) location(ctx context.Context) (string, error) {
	if v := p.cfg.Options[optLocation]; v != "" {
		return v, nil
	}
	if self, err := p.Self(ctx); err == nil && self.Region != "" {
		return self.Region, nil
	}
	return "", fmt.Errorf("location unknown; pass -provider-opt %s=<region>", optLocation)
}

// clientOptions applies an endpoint override when one is set. Overriding also
// disables authentication, because the fake servers these point at have no
// credentials and Application Default Credentials would otherwise hang trying
// the metadata server.
func (p *Provider) clientOptions(key string) []option.ClientOption {
	endpoint := p.cfg.Options[key]
	if endpoint == "" {
		return nil
	}
	return []option.ClientOption{option.WithEndpoint(endpoint), option.WithoutAuthentication()}
}

func (p *Provider) Discovery(ctx context.Context) (provider.Discoverer, error) {
	project, err := p.project(ctx)
	if err != nil {
		return nil, err
	}
	location, err := p.location(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sdLookup == nil {
		c, err := servicedirectory.NewLookupClient(ctx, p.clientOptions(optEndpointD)...)
		if err != nil {
			return nil, fmt.Errorf("service directory: %w", err)
		}
		p.sdLookup = c
	}
	return &Discovery{
		client:    p.sdLookup,
		logger:    p.logger,
		project:   project,
		location:  location,
		namespace: p.cfg.Namespace,
	}, nil
}

func (p *Provider) Registrar(ctx context.Context) (provider.Registrar, error) {
	project, err := p.project(ctx)
	if err != nil {
		return nil, err
	}
	location, err := p.location(ctx)
	if err != nil {
		return nil, err
	}
	self, err := p.Self(ctx)
	if err != nil {
		return nil, fmt.Errorf("registration needs this instance's identity: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sdReg == nil {
		c, err := servicedirectory.NewRegistrationClient(ctx, p.clientOptions(optEndpointD)...)
		if err != nil {
			return nil, fmt.Errorf("service directory: %w", err)
		}
		p.sdReg = c
	}
	return &Registrar{
		client:    p.sdReg,
		logger:    p.logger,
		project:   project,
		location:  location,
		namespace: p.cfg.Namespace,
		// Empty on a service or a job; Register says why rather than
		// publishing an address nothing can connect to.
		address: self.IPv4,
	}, nil
}

func (p *Provider) KV(ctx context.Context) (provider.KVStore, error) {
	if p.cfg.KVStore == "" {
		return nil, fmt.Errorf("%w (set -kv-store)", provider.ErrNotConfigured)
	}
	// Best-effort: outside Cloud Run there is no identity, and OwnerID falls
	// back to the hostname, which is unique enough for a lease owner.
	self, _ := p.Self(ctx)
	owner := provider.OwnerID(self)

	if p.cfg.Options[optKVBackend] == backendFirestore {
		return p.firestoreKV(ctx, owner)
	}
	return p.gcsKV(ctx, owner)
}

func (p *Provider) gcsKV(ctx context.Context, owner string) (provider.KVStore, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gcs == nil {
		c, err := storage.NewClient(ctx, p.clientOptions(optEndpointS)...)
		if err != nil {
			return nil, fmt.Errorf("cloud storage: %w", err)
		}
		p.gcs = c
	}
	// project and location are only needed to create the bucket; an empty
	// value here is fine unless -kv-create is also set, which reports it.
	project, _ := p.project(ctx)
	location, _ := p.location(ctx)

	p.logger.Info("kv store enabled", slog.String("backend", backendGCS),
		slog.String("bucket", p.cfg.KVStore), slog.String("owner", owner))
	return newGCSKV(p.gcs, p.cfg.KVStore, p.cfg.KVPrefix, owner, project, location), nil
}

func (p *Provider) firestoreKV(ctx context.Context, owner string) (provider.KVStore, error) {
	project, err := p.project(ctx)
	if err != nil {
		return nil, err
	}
	database := p.cfg.Options[optKVDatabase]
	if database == "" {
		database = firestore.DefaultDatabaseID
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fs == nil {
		// NewClientWithDatabase covers the default database too, so there is
		// one path rather than two. FIRESTORE_EMULATOR_HOST is honoured by the
		// client itself, which is how the conformance suite reaches an
		// emulator without muster knowing about one.
		c, err := firestore.NewClientWithDatabase(ctx, project, database)
		if err != nil {
			return nil, fmt.Errorf("firestore: %w", err)
		}
		p.fs = c
	}

	p.logger.Info("kv store enabled", slog.String("backend", backendFirestore),
		slog.String("collection", p.cfg.KVStore), slog.String("database", database),
		slog.String("owner", owner))
	return newFirestoreKV(p.fs, p.cfg.KVStore, p.cfg.KVPrefix, owner), nil
}

func init() { provider.Register(Factory{}) }
