package main

import (
	"flag"
	"strings"
	"testing"
)

// testFlags mirrors the shape of the real flag set: a string, a bool, and a
// repeatable Var.
func testFlags() (*flag.FlagSet, *string, *bool, optFlags) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	ns := fs.String("namespace", "", "")
	create := fs.Bool("kv-create", false, "")
	opts := optFlags{}
	fs.Var(opts, "provider-opt", "")
	fs.Bool("health-probe", false, "")
	for old := range renamedFlags {
		fs.String(old, "", "")
	}
	return fs, ns, create, opts
}

func env(pairs map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := pairs[k]; return v, ok }
}

func TestEnvName(t *testing.T) {
	for in, want := range map[string]string{
		"namespace":     "MUSTER_NAMESPACE",
		"kv-store":      "MUSTER_KV_STORE",
		"kv-key-prefix": "MUSTER_KV_KEY_PREFIX",
		"provider":      "MUSTER_PROVIDER",
	} {
		if got := envName(in); got != want {
			t.Errorf("envName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyEnvDefaults(t *testing.T) {
	fs, ns, create, opts := testFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	err := applyEnvDefaults(fs, env(map[string]string{
		"MUSTER_NAMESPACE":    "from-env",
		"MUSTER_KV_CREATE":    "true",
		"MUSTER_PROVIDER_OPT": "project=p, location=asia-northeast1",
	}))
	if err != nil {
		t.Fatalf("applyEnvDefaults: %v", err)
	}
	if *ns != "from-env" {
		t.Errorf("namespace = %q", *ns)
	}
	if !*create {
		t.Error("kv-create should be set from the environment")
	}
	// A repeatable flag takes a comma-separated list, trimmed.
	if opts["project"] != "p" || opts["location"] != "asia-northeast1" {
		t.Errorf("provider-opt = %v", opts)
	}
}

// An explicitly passed flag wins, so a task definition can override the
// deployment's environment without either knowing about the other.
func TestApplyEnvDefaultsFlagWins(t *testing.T) {
	fs, ns, _, _ := testFlags()
	if err := fs.Parse([]string{"-namespace", "from-flag"}); err != nil {
		t.Fatal(err)
	}
	if err := applyEnvDefaults(fs, env(map[string]string{"MUSTER_NAMESPACE": "from-env"})); err != nil {
		t.Fatal(err)
	}
	if *ns != "from-flag" {
		t.Errorf("namespace = %q, want the flag to win", *ns)
	}
}

// A stray variable in a shell must not turn an ordinary run into a probe.
func TestApplyEnvDefaultsSkipsModeSwitches(t *testing.T) {
	fs, _, _, _ := testFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyEnvDefaults(fs, env(map[string]string{"MUSTER_HEALTH_PROBE": "true"})); err != nil {
		t.Fatal(err)
	}
	if fs.Lookup("health-probe").Value.String() != "false" {
		t.Error("health-probe must not be settable from the environment")
	}
}

// The removed flags exist only to report their replacements. An environment
// nobody has cleaned up must not bring one back.
func TestApplyEnvDefaultsSkipsRemovedFlags(t *testing.T) {
	fs, _, _, _ := testFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyEnvDefaults(fs, env(map[string]string{"MUSTER_KV_TABLE": "resurrected"})); err != nil {
		t.Fatal(err)
	}
	if got := fs.Lookup("kv-table").Value.String(); got != "" {
		t.Errorf("kv-table = %q, want the environment ignored for a removed flag", got)
	}
}

// A variable that cannot be parsed is a setting the operator believes is in
// effect, so it fails rather than being dropped.
func TestApplyEnvDefaultsRejectsUnparseable(t *testing.T) {
	fs, _, _, _ := testFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	err := applyEnvDefaults(fs, env(map[string]string{"MUSTER_KV_CREATE": "yes-please"}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "MUSTER_KV_CREATE") {
		t.Errorf("the error should name the variable, got: %v", err)
	}
}

// An empty variable is not a value. Container platforms materialise unset
// variables as empty strings often enough that treating "" as "set" would make
// an absent setting override a flag's default with nothing.
func TestApplyEnvDefaultsIgnoresEmpty(t *testing.T) {
	fs, ns, _, _ := testFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyEnvDefaults(fs, env(map[string]string{"MUSTER_NAMESPACE": ""})); err != nil {
		t.Fatal(err)
	}
	if *ns != "" {
		t.Errorf("namespace = %q", *ns)
	}
}
