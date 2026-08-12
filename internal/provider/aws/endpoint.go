package aws

import (
	"context"

	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// disableEndpointPrefix applies the flag that will prevent any
// operation-specific host prefix from being applied.
//
// It exists for AWS_ENDPOINT_URL: CloudMap's DiscoverInstances is served from
// data-servicediscovery.<region>.amazonaws.com rather than the client's
// configured host, so without this an endpoint override aimed at an emulator
// would still be prefixed and miss it.
type disableEndpointPrefix struct{}

func (disableEndpointPrefix) ID() string { return "disableEndpointPrefix" }

func (disableEndpointPrefix) HandleInitialize(
	ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler,
) (middleware.InitializeOutput, middleware.Metadata, error) {
	ctx = smithyhttp.SetHostnameImmutable(ctx, true)
	return next.HandleInitialize(ctx, in)
}

// AddDisableEndpointPrefix registers the middleware on a client's stack.
func AddDisableEndpointPrefix(stack *middleware.Stack) error {
	return stack.Initialize.Add(disableEndpointPrefix{}, middleware.After)
}
