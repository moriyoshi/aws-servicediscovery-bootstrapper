package aws

import "github.com/moriyoshi/muster/internal/provider"

func init() { provider.Register(Factory{}) }
