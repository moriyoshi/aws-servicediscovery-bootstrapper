package main

import (
	"flag"
	"fmt"
	"strings"
)

// muster is a container entrypoint, and a container's entrypoint is baked into
// its image while its environment is per deployment. So every flag can also be
// supplied as MUSTER_<FLAG>, with the dashes turned into underscores:
//
//	-namespace       MUSTER_NAMESPACE
//	-kv-store        MUSTER_KV_STORE
//	-kv-key-prefix   MUSTER_KV_KEY_PREFIX
//
// An explicitly passed flag always wins, so a task definition can override the
// deployment's environment without either having to know about the other.
//
// This started as a special case for MUSTER_PROVIDER, which needed it because
// the provider is chosen per deployment. Every other setting has the same
// shape, and having exactly one of them honour the environment was a trap: an
// image whose entrypoint ends in `-- <workload>` cannot take extra flags by
// appending arguments, so a platform that only lets you set environment and
// arguments leaves no way to pass one.

// envExempt lists flags the environment must not be able to set.
//
// The two mode switches, because a stray variable in a shell would otherwise
// turn an ordinary run into a health probe or a help dump; and the removed
// flags, which exist only to report their replacements and should not be
// resurrectable by an environment nobody has cleaned up.
var envExempt = map[string]bool{
	"health-probe":  true,
	"provider-help": true,
}

// envRepeatable lists flags that may be given more than once. Their variable
// holds a comma-separated list, each element passed to the flag in turn.
var envRepeatable = map[string]bool{
	"provider-opt": true,
}

// envName is the variable that can supply a flag: -kv-store <- MUSTER_KV_STORE.
func envName(flagName string) string {
	return "MUSTER_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// applyEnvDefaults fills flags that were not passed on the command line from the
// environment. lookup is os.LookupEnv in production.
func applyEnvDefaults(fs *flag.FlagSet, lookup func(string) (string, bool)) error {
	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })

	var firstErr error
	fs.VisitAll(func(f *flag.Flag) {
		if firstErr != nil || passed[f.Name] || envExempt[f.Name] || renamedFlags[f.Name] != "" {
			return
		}
		raw, ok := lookup(envName(f.Name))
		if !ok || raw == "" {
			return
		}
		values := []string{raw}
		if envRepeatable[f.Name] {
			values = strings.Split(raw, ",")
		}
		for _, v := range values {
			if err := f.Value.Set(strings.TrimSpace(v)); err != nil {
				// Reported rather than ignored: a variable that cannot be
				// parsed is a setting the operator believes is in effect.
				firstErr = fmt.Errorf("%s: %w", envName(f.Name), err)
				return
			}
		}
	})
	return firstErr
}

// envSettableFlags lists the variables that can configure this binary, for the
// end-to-end stacks' own check that everything they set is read by something.
func envSettableFlags(fs *flag.FlagSet) []string {
	var out []string
	fs.VisitAll(func(f *flag.Flag) {
		if envExempt[f.Name] || renamedFlags[f.Name] != "" {
			return
		}
		out = append(out, envName(f.Name))
	})
	return out
}
