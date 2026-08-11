package main

import (
	"testing"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
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
	v := entryToStarlark(entry{IPv4Addr: "10.0.0.1", IPv6Addr: "::1", Port: 2379})
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

func TestTaskMetaToStarlark(t *testing.T) {
	m := &taskMetadataV4{Cluster: "c", ServiceName: "svc", AvailabilityZone: "us-east-1a", TaskARN: "arn"}
	s := taskMetaToStarlark(m).(*starlarkstruct.Struct)
	az, _ := s.Attr("availability_zone")
	if az.(starlark.String) != "us-east-1a" {
		t.Fatalf("az=%v", az)
	}
	if taskMetaToStarlark(nil) != starlark.None {
		t.Fatal("nil metadata should map to None")
	}
}
