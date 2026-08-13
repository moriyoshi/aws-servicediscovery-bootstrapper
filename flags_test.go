package main

import (
	"strings"
	"testing"

	"github.com/moriyoshi/muster/internal/provider"
	"github.com/moriyoshi/muster/internal/provider/memkv"
)

func TestOptFlagsSet(t *testing.T) {
	m := optFlags{}
	if err := m.Set("project=p1"); err != nil {
		t.Fatalf("k=v: %v", err)
	}
	// An empty value is meaningful -- it can override a default with "unset".
	if err := m.Set("location="); err != nil {
		t.Fatalf("k=: %v", err)
	}
	if m["project"] != "p1" || m["location"] != "" {
		t.Fatalf("got %v", m)
	}
	if err := m.Set("bare"); err == nil {
		t.Error("a value with no = should be rejected")
	}
	if err := m.Set("=v"); err == nil {
		t.Error("an empty key should be rejected")
	}
	// Silently keeping one of two values for the same key is the kind of thing
	// nobody notices until the wrong one is in effect.
	if err := m.Set("project=p2"); err == nil {
		t.Error("a duplicate key should be rejected")
	}
	if got := m.String(); got != "location=,project=p1" {
		t.Errorf("String() = %q", got)
	}
}

// A removed flag has to name its replacement. flag's own "flag provided but not
// defined" cannot, and these two live in a task definition that can lag the
// image, so the message is all an operator has to go on.
func TestCheckRenamedFlags(t *testing.T) {
	err := checkRenamedFlags(func(name string) bool { return name == "kv-table" })
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "-kv-store") {
		t.Errorf("error should name the replacement, got: %v", err)
	}

	if err := checkRenamedFlags(func(string) bool { return false }); err != nil {
		t.Errorf("no removed flag passed, got: %v", err)
	}
}

// Which providers are linked depends on the build tags, so the invariant is
// that -provider-help names every provider this binary knows about -- compiled
// in or not -- rather than any particular set.
func TestProviderHelpNamesEveryKnownProvider(t *testing.T) {
	help := providerHelp()
	for _, name := range provider.Known() {
		if !strings.Contains(help, name) {
			t.Errorf("-provider-help should mention %q, got:\n%s", name, help)
		}
	}
	// The in-process provider has no build constraint, so it is always there and
	// every build has at least one working provider.
	if !strings.Contains(help, memkv.Name) {
		t.Errorf("-provider-help should always list %q, got:\n%s", memkv.Name, help)
	}
	if len(provider.Known()) == len(provider.Available()) {
		t.Skip("this build has every known provider compiled in")
	}
	if !strings.Contains(help, "known but not built in") {
		t.Errorf("-provider-help should flag providers that are not built in, got:\n%s", help)
	}
}
