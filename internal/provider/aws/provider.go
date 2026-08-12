package aws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"

	"github.com/moriyoshi/muster/internal/provider"
)

// Factory builds the AWS provider. Region, credentials and AWS_ENDPOINT_URL all
// come from the SDK's own default chain, so there is nothing here to configure
// with -provider-opt.
type Factory struct{}

var _ provider.Factory = Factory{}

func (Factory) Name() string { return Name }

func (Factory) Options() []provider.OptionSpec { return nil }

func (Factory) Open(ctx context.Context, cfg provider.Config) (provider.Provider, error) {
	if err := provider.ValidateOptions(cfg.Options, Factory{}.Options()); err != nil {
		return nil, err
	}
	if cfg.Namespace == "" {
		return nil, errors.New("namespace is required")
	}
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	p := &Provider{cfg: cfg, aws: awsCfg, logger: cfg.Logger}

	loggerOpts := []any{
		slog.String("provider", Name),
		slog.String("aws_region", awsCfg.Region),
		slog.String("namespace", cfg.Namespace),
	}
	if awsCfg.BaseEndpoint != nil {
		loggerOpts = append(loggerOpts, slog.String("aws_endpoint", *awsCfg.BaseEndpoint))
	}
	cfg.Logger.Info("service discovery will be performed", loggerOpts...)

	return p, nil
}

// Provider is the AWS implementation of muster's capabilities. Each accessor
// builds its client on first use and memoizes it.
type Provider struct {
	provider.Unimplemented

	cfg    provider.Config
	aws    awssdk.Config
	logger *slog.Logger

	selfOnce sync.Once
	self     *provider.Identity
	selfErr  error
}

var _ provider.Provider = (*Provider)(nil)

func (p *Provider) Name() string { return Name }

func (p *Provider) Discovery(context.Context) (provider.Discoverer, error) {
	// AWS_ENDPOINT_URL aims every client at one host, but CloudMap serves
	// DiscoverInstances from data-servicediscovery.<region>.amazonaws.com. Left
	// alone, the SDK would prefix the override too and miss the emulator.
	options := make([]func(*servicediscovery.Options), 0, 1)
	if p.aws.BaseEndpoint != nil {
		options = append(options, servicediscovery.WithAPIOptions(AddDisableEndpointPrefix))
	}
	return NewServiceDiscovery(servicediscovery.NewFromConfig(p.aws, options...), p.cfg.Namespace), nil
}

func (p *Provider) Fleet(context.Context) (provider.Fleet, error) {
	return NewFleet(ecs.NewFromConfig(p.aws)), nil
}

// Self is memoized because KV derives its lease owner from it: resolving
// identity lazily after the store was built would produce leases whose owner
// does not match, and Renew would fail for the lifetime of the process.
func (p *Provider) Self(ctx context.Context) (*provider.Identity, error) {
	p.selfOnce.Do(func() { p.self, p.selfErr = FetchIdentity(ctx) })
	return p.self, p.selfErr
}

func (p *Provider) KV(ctx context.Context) (provider.KVStore, error) {
	if p.cfg.KVStore == "" {
		return nil, fmt.Errorf("%w (set -kv-store)", provider.ErrNotConfigured)
	}
	// Best-effort: outside ECS there is no identity, and OwnerID falls back to
	// the hostname, which is unique enough for a lease owner.
	self, _ := p.Self(ctx)
	owner := provider.OwnerID(self)

	kv := NewDynamoKV(dynamodb.NewFromConfig(p.aws), p.cfg.KVStore, p.cfg.KVPrefix, owner)
	p.logger.Info("kv store enabled",
		slog.String("table", p.cfg.KVStore), slog.String("owner", owner))
	return kv, nil
}
