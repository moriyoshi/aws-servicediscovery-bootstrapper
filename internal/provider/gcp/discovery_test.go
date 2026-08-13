//go:build gcp

package gcp

import "testing"

// Service Directory allows 63 characters for an endpoint id, and a Cloud Run
// instance id is around two hundred hex digits. Registering under one fails
// with "Invalid endpoint name" -- and fails *quietly* in effect, because a
// script only learns of it when instances() returns nothing and every peer
// looks absent. That is what took a whole provisioned stack to discover.
func TestEndpointID(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"10.128.253.20", "ip-10-128-253-20"},
		{"192.168.0.1", "ip-192-168-0-1"},
		{"fd00::1", "ip-fd00--1"},
		{"2001:db8:85a3::8a2e:370:7334", "ip-2001-db8-85a3--8a2e-370-7334"},
	} {
		got := endpointID(tc.addr)
		if got != tc.want {
			t.Errorf("endpointID(%q) = %q, want %q", tc.addr, got, tc.want)
		}
		assertValid(t, got)
	}
}

// The id has to stay valid whatever it is handed, because an invalid one is not
// rejected until it reaches the API and then only looks like an absent peer.
func TestEndpointIDIsAlwaysValid(t *testing.T) {
	for _, in := range []string{
		"",
		"UPPER.CASE.1",
		"has_underscores",
		// An instance id, which is what this used to be keyed on.
		"001548f729d7f0cd66f732c5cae8882c795166b46efc74091e45cb9c3db12c8f7d232db1b838685c64da85d047440400b95f66e07b7a78a5bfe19a745beac513e7b33268e69b8f7cbe4f6dfb9f08f5a3d7e3d4ba19e74e36e49ef3a6",
		"trailing...",
	} {
		assertValid(t, endpointID(in))
	}
}

func assertValid(t *testing.T, id string) {
	t.Helper()
	if id == "" || len(id) > 63 {
		t.Errorf("endpoint id %q has length %d, want 1..63", id, len(id))
		return
	}
	if c := id[0]; c < 'a' || c > 'z' {
		t.Errorf("endpoint id %q must start with a letter", id)
	}
	if c := id[len(id)-1]; !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
		t.Errorf("endpoint id %q must end with a letter or digit", id)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			t.Errorf("endpoint id %q has an invalid character %q", id, c)
			return
		}
	}
}
