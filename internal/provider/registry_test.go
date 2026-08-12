package provider

import (
	"context"
	"strings"
	"testing"
)

type fakeFactory struct{ name string }

func (f fakeFactory) Name() string        { return f.name }
func (fakeFactory) Options() []OptionSpec { return nil }
func (f fakeFactory) Open(context.Context, Config) (Provider, error) {
	return Unimplemented{ProviderName: f.name}, nil
}

// withRegistry swaps in an isolated registry so these tests do not depend on
// which providers happen to be linked into the test binary.
func withRegistry(t *testing.T, present []string, missing map[string]string) {
	t.Helper()
	regMu.Lock()
	oldF, oldA := factories, absent
	factories, absent = map[string]Factory{}, map[string]string{}
	for _, n := range present {
		factories[n] = fakeFactory{name: n}
	}
	for n, reason := range missing {
		absent[n] = reason
	}
	regMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		factories, absent = oldF, oldA
		regMu.Unlock()
	})
}

func clearDetectEnv(t *testing.T) {
	t.Helper()
	for _, d := range detectors {
		for _, name := range d.env {
			t.Setenv(name, "")
		}
	}
}

func TestSelectCompiledIn(t *testing.T) {
	withRegistry(t, []string{"aws", "mem"}, nil)
	f, err := Select("mem")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if f.Name() != "mem" {
		t.Fatalf("got %q", f.Name())
	}
}

// Selecting a provider that exists in the tree but not in this binary has to
// say how to get it. "unknown provider" would read as a typo and send the
// operator looking in the wrong place.
func TestSelectAbsentProviderExplainsHowToGetIt(t *testing.T) {
	withRegistry(t, []string{"aws"}, map[string]string{
		"gcp": "this muster was built without gcp support; rebuild with `go build -tags gcp`",
	})
	_, err := Select("gcp")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "-tags gcp") {
		t.Fatalf("error should say how to rebuild, got: %v", err)
	}
}

func TestSelectUnknownProviderListsKnown(t *testing.T) {
	withRegistry(t, []string{"aws"}, map[string]string{"gcp": "not built in"})
	_, err := Select("azure")
	if err == nil {
		t.Fatal("expected an error")
	}
	// Both the compiled-in and the absent names are worth listing: the operator
	// needs to see the whole vocabulary, not just this build's half of it.
	for _, want := range []string{"aws", "gcp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list %q, got: %v", want, err)
		}
	}
}

func TestAutodetectFromECSMetadataEnv(t *testing.T) {
	withRegistry(t, []string{"aws"}, nil)
	for _, name := range []string{"ECS_CONTAINER_METADATA_URI_V4", "ECS_CONTAINER_METADATA_URI"} {
		t.Run(name, func(t *testing.T) {
			clearDetectEnv(t)
			t.Setenv(name, "http://169.254.170.2/v4/abc")

			f, err := Select("")
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if f.Name() != "aws" {
				t.Fatalf("got %q", f.Name())
			}
		})
	}
}

// Autodetection has to answer for providers this binary lacks, or an AWS-only
// muster on another platform would fail much later with something unrelated.
func TestAutodetectReachesAnAbsentProvider(t *testing.T) {
	withRegistry(t, nil, map[string]string{"aws": "rebuild without -tags gcp"})
	clearDetectEnv(t)
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "http://169.254.170.2/v4/abc")

	_, err := Select("")
	if err == nil || !strings.Contains(err.Error(), "rebuild") {
		t.Fatalf("expected the rebuild instruction, got: %v", err)
	}
}

func TestSelectWithNothingToGoOn(t *testing.T) {
	withRegistry(t, []string{"aws", "mem"}, nil)
	clearDetectEnv(t)

	_, err := Select("")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"-provider", envProvider, "aws, mem"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestValidateOptions(t *testing.T) {
	specs := []OptionSpec{{Key: "project"}, {Key: "location"}}

	if err := ValidateOptions(map[string]string{"project": "p"}, specs); err != nil {
		t.Fatalf("declared key: %v", err)
	}
	// A typo has to fail. An option that is quietly ignored looks applied.
	err := ValidateOptions(map[string]string{"projet": "p"}, specs)
	if err == nil {
		t.Fatal("expected an error for an undeclared key")
	}
	for _, want := range []string{"projet", "location, project"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
	if err := ValidateOptions(map[string]string{"x": "1"}, nil); err == nil ||
		!strings.Contains(err.Error(), "takes no -provider-opt") {
		t.Errorf("a factory with no options should say so, got: %v", err)
	}
	if err := ValidateOptions(nil, nil); err != nil {
		t.Errorf("no options at all: %v", err)
	}
}
