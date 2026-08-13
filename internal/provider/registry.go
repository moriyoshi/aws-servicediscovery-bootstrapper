package provider

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

var (
	regMu     sync.Mutex
	factories = map[string]Factory{}
	absent    = map[string]string{}
)

// Register makes a provider selectable. It is called from an init() in a
// build-tagged registration file, so which providers exist is decided when the
// binary is built.
func Register(f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	name := f.Name()
	if _, dup := factories[name]; dup {
		// A duplicate can only come from two registration files being compiled
		// together, which is a build-tag mistake rather than a runtime
		// condition.
		panic(fmt.Sprintf("provider %q registered twice", name))
	}
	factories[name] = f
}

// RegisterAbsent records a provider this binary knows about but was not built
// with, together with what to do about it. Without this, selecting a provider
// that exists in the source tree but not in this binary would report "unknown
// provider", which reads like a typo rather than a build option.
func RegisterAbsent(name, reason string) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, present := factories[name]; present {
		return
	}
	absent[name] = reason
}

// Available lists the providers compiled into this binary.
func Available() []string {
	regMu.Lock()
	defer regMu.Unlock()
	return sortedKeys(factories)
}

// Known lists every provider this binary can name, compiled in or not.
func Known() []string {
	regMu.Lock()
	defer regMu.Unlock()
	names := append(sortedKeys(factories), sortedKeys(absent)...)
	sort.Strings(names)
	return names
}

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Select resolves a provider name to its factory. An empty name means "work it
// out", which is Autodetect plus a diagnosis when it cannot.
func Select(name string) (Factory, error) {
	if name == "" {
		detected, ok := Autodetect()
		if !ok {
			return nil, fmt.Errorf(
				"could not determine the provider from the environment; pass -provider or set %s (available: %s)",
				envProvider, strings.Join(Available(), ", "))
		}
		name = detected
	}

	regMu.Lock()
	defer regMu.Unlock()
	if f, ok := factories[name]; ok {
		return f, nil
	}
	if reason, ok := absent[name]; ok {
		return nil, fmt.Errorf("provider %q: %s", name, reason)
	}
	names := append(sortedKeys(factories), sortedKeys(absent)...)
	sort.Strings(names)
	return nil, fmt.Errorf("unknown provider %q (known: %s)", name, strings.Join(names, ", "))
}

// envProvider is the variable that supplies -provider when it is not passed.
// The harness fills the flag from it, along with every other flag's variable;
// the name appears here only so the diagnosis below can suggest it.
const envProvider = "MUSTER_PROVIDER"

// detectors maps a provider to environment variables whose presence means we
// are running on it.
//
// The table lives here rather than on Factory so that it can answer for
// providers this binary was *not* built with: an AWS-only muster started on a
// platform it cannot serve should say so, not fail twenty seconds later on a
// credentials error. That is also why the variable names are repeated here
// instead of imported from the provider package -- importing it would link it.
//
// Detection never dials anything. A metadata-server probe would cost a timeout
// on every container start on every other platform, so a provider with no
// distinguishing environment variable is simply not autodetectable and has to
// be named explicitly.
var detectors = []struct {
	name string
	env  []string
}{
	// ECS advertises its task metadata endpoint. See the aws package for why
	// these are ECS_-prefixed and not AWS_-prefixed.
	{"aws", []string{"ECS_CONTAINER_METADATA_URI_V4", "ECS_CONTAINER_METADATA_URI"}},
	// Cloud Run sets K_SERVICE on a service, CLOUD_RUN_JOB on a job and
	// CLOUD_RUN_WORKER_POOL on a worker pool.
	{"gcp", []string{"K_SERVICE", "CLOUD_RUN_JOB", "CLOUD_RUN_WORKER_POOL"}},
}

// Autodetect returns the provider the environment points at, whether or not it
// is compiled in; Select turns an absent match into the rebuild instruction.
func Autodetect() (string, bool) {
	for _, d := range detectors {
		for _, name := range d.env {
			if os.Getenv(name) != "" {
				return d.name, true
			}
		}
	}
	return "", false
}
