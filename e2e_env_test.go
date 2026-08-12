package main

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// tfEnvName matches the two ways the stacks declare a container variable:
//
//	{ name = "MUSTER_X", value = ... }   (ECS task definition)
//	MUSTER_X = ...                       (a Terraform map)
var tfEnvName = regexp.MustCompile(`(?m)name\s*=\s*"(MUSTER_[A-Z0-9_]+)"|^\s*(MUSTER_[A-Z0-9_]+)\s*=`)

// starEnvName matches env("MUSTER_X") and env('MUSTER_X') in a script.
var starEnvName = regexp.MustCompile(`env\(\s*["'](MUSTER_[A-Z0-9_]+)["']`)

// TestE2EStackEnvIsConsumed checks that every MUSTER_* variable an end-to-end
// stack sets on a container is read by something: muster itself, or one of the
// Starlark scripts that stack deploys.
//
// This exists because the Google Cloud stack once passed MUSTER_KV_BUCKET,
// which is not what the flag is called, so the configuration was simply absent
// and PD refused to start -- a failure that cost a full provision to discover
// and would have been a one-line diff to prevent. Every flag now has a
// variable, which makes the near-miss names the remaining hazard: MUSTER_KV_BUCKET
// looks exactly as plausible as MUSTER_KV_STORE.
//
// It deliberately does not check the converse. A script may read a variable the
// stack does not set: env() takes a default, and both PD scripts rely on that.
func TestE2EStackEnvIsConsumed(t *testing.T) {
	for _, suite := range []string{"e2e/tikv/aws", "e2e/tikv/gcp"} {
		t.Run(strings.TrimPrefix(suite, "e2e/"), func(t *testing.T) {
			set := namesSetBy(t, filepath.Join(suite, "terraform"))
			if len(set) == 0 {
				t.Fatalf("%s: found no MUSTER_* variables at all; the pattern has stopped matching", suite)
			}
			read := namesReadBy(t, filepath.Join(suite, "docker"))

			for _, name := range envSettableFlags(flag.CommandLine) {
				read[name] = "a muster flag"
			}
			var orphans []string
			for name := range set {
				if _, ok := read[name]; !ok {
					orphans = append(orphans, name)
				}
			}
			sort.Strings(orphans)
			for _, name := range orphans {
				t.Errorf("%s sets %s, but neither muster nor any script it deploys reads it; "+
					"muster's own configuration travels as flags", suite, name)
			}
		})
	}
}

func namesSetBy(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	walkFiles(t, dir, ".tf", func(path, body string) {
		for _, m := range tfEnvName.FindAllStringSubmatch(body, -1) {
			for _, name := range m[1:] {
				if name != "" {
					out[name] = true
				}
			}
		}
	})
	return out
}

func namesReadBy(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	walkFiles(t, dir, ".star", func(path, body string) {
		for _, m := range starEnvName.FindAllStringSubmatch(body, -1) {
			out[m[1]] = path
		}
	})
	return out
}

func walkFiles(t *testing.T, dir, ext string, fn func(path, body string)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// .terraform holds a downloaded provider, not configuration.
		if d.IsDir() && d.Name() == ".terraform" {
			return filepath.SkipDir
		}
		if d.IsDir() || filepath.Ext(path) != ext {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fn(path, string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// The stacks configure muster through the environment, so the variables have to
// exist for the flags they mean to set.
func TestStackFlagsHaveEnvNames(t *testing.T) {
	names := map[string]bool{}
	for _, n := range envSettableFlags(flag.CommandLine) {
		names[n] = true
	}
	for _, want := range []string{
		"MUSTER_PROVIDER", "MUSTER_NAMESPACE", "MUSTER_SCRIPT",
		"MUSTER_KV_STORE", "MUSTER_KV_KEY_PREFIX", "MUSTER_CONTROL_SOCKET",
	} {
		if !names[want] {
			t.Errorf("%s is not settable from the environment; a stack that sets it "+
				"would be configuring nothing", want)
		}
	}
}
