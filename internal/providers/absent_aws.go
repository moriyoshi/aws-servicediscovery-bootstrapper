//go:build gcp

package providers

import "github.com/moriyoshi/muster/internal/provider"

func init() {
	provider.RegisterAbsent("aws",
		"this muster was built for another cloud; rebuild without -tags gcp")
}
