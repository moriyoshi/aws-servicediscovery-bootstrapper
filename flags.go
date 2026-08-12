package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/moriyoshi/muster/internal/provider"
)

// optFlags collects repeated -provider-opt k=v into a map.
//
// Provider-specific settings arrive this way rather than as their own flags
// (-gcp-project and friends) because flag.Parse runs before the provider is
// selected. Registering one cloud's flags in a binary built for another would
// make `-gcp-project x` fail with "flag provided but not defined" -- the
// un-actionable error the provider registry exists to avoid -- while
// pre-registering every cloud's flags in every binary defeats the point of
// building them separately. Values stay opaque strings until the chosen factory
// validates them against its Options.
type optFlags map[string]string

func (m optFlags) String() string {
	if len(m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (m optFlags) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("expected k=v, got %q", s)
	}
	if _, dup := m[k]; dup {
		return fmt.Errorf("duplicate -provider-opt key %q", k)
	}
	m[k] = v
	return nil
}

// providerHelp describes the compiled-in providers and the options each accepts,
// which is the discoverability -provider-opt would otherwise cost.
func providerHelp() string {
	var b strings.Builder
	available := provider.Available()
	fmt.Fprintf(&b, "providers compiled into this muster: %s\n", strings.Join(available, ", "))

	built := make(map[string]bool, len(available))
	for _, n := range available {
		built[n] = true
	}
	var missing []string
	for _, n := range provider.Known() {
		if !built[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, "known but not built in (pass -provider <name> to see how to get one): %s\n",
			strings.Join(missing, ", "))
	}

	for _, name := range available {
		f, err := provider.Select(name)
		if err != nil {
			continue
		}
		opts := f.Options()
		fmt.Fprintf(&b, "\n%s\n", name)
		if len(opts) == 0 {
			fmt.Fprintf(&b, "  (no -provider-opt keys)\n")
			continue
		}
		for _, o := range opts {
			fmt.Fprintf(&b, "  %-26s %s", o.Key, o.Doc)
			if o.Default != "" {
				fmt.Fprintf(&b, " (default %q)", o.Default)
			}
			fmt.Fprintln(&b)
		}
	}
	return b.String()
}

// renamedFlags are flags that no longer exist, kept registered only so that
// passing one reports its replacement.
//
// This is diagnosability, not compatibility: the old name never works. It earns
// its keep because these flags live in a task definition rather than in the
// image, so they are the part of a deployment that can lag the binary -- and
// flag's own "flag provided but not defined" cannot name a replacement, which
// leaves an operator staring at a container that exits 2 as PID 1 on every
// replica at once.
var renamedFlags = map[string]string{
	"kv-table":        "kv-store",
	"kv-create-table": "kv-create",
}

// checkRenamedFlags reports the first removed flag that was actually passed.
func checkRenamedFlags(passed func(string) bool) error {
	names := make([]string, 0, len(renamedFlags))
	for old := range renamedFlags {
		names = append(names, old)
	}
	sort.Strings(names)
	for _, old := range names {
		if passed(old) {
			return fmt.Errorf("-%s has been renamed to -%s (see Migrating in the README)", old, renamedFlags[old])
		}
	}
	return nil
}
