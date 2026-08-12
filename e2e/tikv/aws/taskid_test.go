package aws

import "testing"

func TestShortTaskID(t *testing.T) {
	for in, want := range map[string]string{
		"arn:aws:ecs:us-east-1:123456789012:task/muster-e2e-tikv-main/abc123": "abc123",
		"abc123": "abc123",
	} {
		if got := shortTaskID(in); got != want {
			t.Errorf("shortTaskID(%q) = %q, want %q", in, got, want)
		}
	}
}
