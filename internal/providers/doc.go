// Package providers decides which cloud providers this binary can talk to.
//
// Each provider is compiled in by a blank import guarded by a build tag, and
// the ones that are not get a RegisterAbsent stub so selecting them explains
// how to get them rather than reporting an unknown name. The tags are mutually
// exclusive by construction, so a build always has exactly one cloud provider
// and its image never carries another cloud's SDK.
//
// Import it for side effects only:
//
//	import _ "github.com/moriyoshi/muster/internal/providers"
package providers

// The in-process provider has no dependencies and is always available, so every
// build has at least one working provider even with no cloud configured.
import _ "github.com/moriyoshi/muster/internal/provider/memkv"
