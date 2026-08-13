// Package execout parses the transcript of a command run inside a cluster
// instance, for both the AWS and Google Cloud end-to-end suites.
//
// Neither cluster is reachable from outside its network, so the tests query PD
// by running curl on an instance — `aws ecs execute-command` on one side,
// `gcloud compute ssh --tunnel-through-iap` on the other. Both hand back a
// transcript rather than a response: session banners, tunnel diagnostics, the
// command's own output, and a closing line, some of it over a pty that rewrites
// newlines. `aws ecs execute-command` additionally exits 0 whatever the remote
// command did, so the exit status has to travel in band.
//
// The remote command therefore fences its output:
//
//	echo MUSTER_E2E_BEGIN
//	curl …
//	rc=$?; echo; echo MUSTER_E2E_STATUS=$rc
//	echo MUSTER_E2E_END
//
// This package is deliberately outside the e2e build tags: it is the one piece
// of the harnesses with real parsing logic, and it costs nothing to test.
package execout

import (
	"fmt"
	"strings"
)

const (
	Begin  = "MUSTER_E2E_BEGIN"
	End    = "MUSTER_E2E_END"
	Status = "MUSTER_E2E_STATUS"
)

// Script wraps a shell command in the fence the parser expects.
func Script(command string) string {
	return fmt.Sprintf("echo %s; %s; rc=$?; echo; echo %s=$rc; echo %s",
		Begin, command, Status, End)
}

// Parse extracts the fenced payload and the remote exit status from a session
// transcript. ok is false when the transcript carries no fence at all, which
// means the session never ran the command — a connection failure rather than a
// command failure, and worth retrying.
func Parse(out string) (body []byte, status int, ok bool) {
	// The pty applies ONLCR on the way out; nothing downstream wants the \r.
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = strings.ReplaceAll(out, "\r", "")

	begin := strings.Index(out, Begin+"\n")
	if begin < 0 {
		return nil, 0, false
	}
	rest := out[begin+len(Begin)+1:]
	end := strings.Index(rest, End)
	if end < 0 {
		return nil, 0, false
	}
	payload := rest[:end]

	// Last, not first: a body that happens to contain the marker text must not
	// be able to truncate itself.
	marker := strings.LastIndex(payload, Status+"=")
	if marker < 0 {
		return nil, 0, false
	}
	if _, err := fmt.Sscanf(payload[marker:], Status+"=%d", &status); err != nil {
		return nil, 0, false
	}
	return []byte(strings.TrimSpace(payload[:marker])), status, true
}

// Collapse squeezes a transcript onto one line for an error message.
func Collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
