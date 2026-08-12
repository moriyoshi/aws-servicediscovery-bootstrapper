package main

import (
	"testing"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"github.com/moriyoshi/muster/internal/provider"
)

func TestUnpackDuration(t *testing.T) {
	cases := []struct {
		in   starlark.Value
		want time.Duration
	}{
		{nil, 0},
		{starlark.None, 0},
		{starlark.MakeInt(5), 5 * time.Second},
		{starlark.String("500ms"), 500 * time.Millisecond},
		{starlark.String("2s"), 2 * time.Second},
		{starlark.Float(1.5), 1500 * time.Millisecond},
	}
	for _, c := range cases {
		got, err := unpackDuration(c.in)
		if err != nil {
			t.Fatalf("unpackDuration(%v): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("unpackDuration(%v)=%v want %v", c.in, got, c.want)
		}
	}
	if _, err := unpackDuration(starlark.String("nonsense")); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestStarlarkToStrings(t *testing.T) {
	list := starlark.NewList([]starlark.Value{starlark.String("a"), starlark.String("b")})
	got, err := starlarkToStrings(list)
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %v err %v", got, err)
	}
	bad := starlark.NewList([]starlark.Value{starlark.MakeInt(1)})
	if _, err := starlarkToStrings(bad); err == nil {
		t.Fatal("expected error for non-string element")
	}
}

func TestStarlarkDictToEnv(t *testing.T) {
	d := starlark.NewDict(2)
	d.SetKey(starlark.String("FOO"), starlark.String("bar"))
	env, err := starlarkDictToEnv(d)
	if err != nil || len(env) != 1 || env[0] != "FOO=bar" {
		t.Fatalf("got %v err %v", env, err)
	}
}

func TestEntryToStarlark(t *testing.T) {
	v := entryToStarlark(provider.Instance{IPv4Addr: "10.0.0.1", IPv6Addr: "::1", Port: 2379})
	s, ok := v.(*starlarkstruct.Struct)
	if !ok {
		t.Fatalf("not a struct: %T", v)
	}
	ipv4, err := s.Attr("ipv4")
	if err != nil || ipv4.(starlark.String) != "10.0.0.1" {
		t.Fatalf("ipv4=%v err=%v", ipv4, err)
	}
	port, err := s.Attr("port")
	if err != nil {
		t.Fatal(err)
	}
	if i, _ := port.(starlark.Int).Int64(); i != 2379 {
		t.Fatalf("port=%v", port)
	}
}

func TestSelfToStarlark(t *testing.T) {
	m := &provider.Identity{
		Provider:  "aws",
		ID:        "arn",
		Group:     "c",
		Service:   "svc",
		Zone:      "us-east-1a",
		Region:    "us-east-1",
		Network:   "vpc-1",
		IPv4:      "10.0.2.106",
		CreatedAt: "2026-08-11T15:00:00Z",
		Extra:     map[string]string{"family": "fam", "revision": "7"},
	}
	s := selfToStarlark(m).(*starlarkstruct.Struct)
	for field, want := range map[string]string{
		"id":         "arn",
		"name":       "",
		"group":      "c",
		"service":    "svc",
		"zone":       "us-east-1a",
		"region":     "us-east-1",
		"network":    "vpc-1",
		"ipv4":       "10.0.2.106",
		"ipv6":       "",
		"created_at": "2026-08-11T15:00:00Z",
	} {
		got, err := s.Attr(field)
		if err != nil {
			t.Errorf("%s: %v", field, err)
			continue
		}
		if string(got.(starlark.String)) != want {
			t.Errorf("%s = %v, want %q", field, got, want)
		}
	}

	// Provider-specific fields hang off a sub-struct named for the provider, so
	// a script that reads one is visibly not portable.
	sub, err := s.Attr("aws")
	if err != nil {
		t.Fatalf("SELF.aws: %v", err)
	}
	family, err := sub.(*starlarkstruct.Struct).Attr("family")
	if err != nil || string(family.(starlark.String)) != "fam" {
		t.Fatalf("SELF.aws.family = %v, err %v", family, err)
	}
	// Reading it on a build for another cloud has to fail loudly rather than
	// return something plausible.
	if _, err := s.Attr("gcp"); err == nil {
		t.Fatal("SELF.gcp should not exist on an aws identity")
	}

	// Scripts guard with `if SELF:`, so absent identity has to stay None: a
	// struct of empty strings would be truthy and silently kill the guard.
	if selfToStarlark(nil) != starlark.None {
		t.Fatal("nil identity should map to None")
	}
}
