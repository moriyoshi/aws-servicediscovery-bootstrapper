//go:build !gcp

package providers

import "github.com/moriyoshi/muster/internal/provider"

func init() {
	provider.RegisterAbsent("gcp",
		"this muster was built without gcp support; rebuild with `go build -tags gcp`")
}
