package tikv

import (
	"fmt"
	"strings"
)

// The cluster is not reachable from outside the VPC, so the test queries PD by
// running curl inside a task with `aws ecs execute-command`. That session hands
// back a transcript, not a response: SSM's own banner, the command's stdout and
// stderr, and the plugin's closing line, all multiplexed over a pty that
// rewrites newlines. And `aws ecs execute-command` exits 0 whatever the remote
// command did, so the exit status has to travel in band too.
//
// The remote command therefore fences its output:
//
//	echo MUSTER_E2E_BEGIN
//	curl …
//	rc=$?; echo; echo MUSTER_E2E_STATUS=$rc
//	echo MUSTER_E2E_END
//
// This file is deliberately outside the e2e_tikv build tag: it is the one piece
// of the harness with real parsing logic, and it costs nothing to test.
const (
	execBegin  = "MUSTER_E2E_BEGIN"
	execEnd    = "MUSTER_E2E_END"
	execStatus = "MUSTER_E2E_STATUS"
)

// parseExecOutput extracts the fenced payload and the remote exit status from a
// session transcript. ok is false when the transcript carries no fence at all,
// which means the session never ran the command — a connection failure rather
// than a command failure, and worth retrying.
func parseExecOutput(out string) (body []byte, status int, ok bool) {
	// The pty applies ONLCR on the way out; nothing downstream wants the \r.
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = strings.ReplaceAll(out, "\r", "")

	begin := strings.Index(out, execBegin+"\n")
	if begin < 0 {
		return nil, 0, false
	}
	rest := out[begin+len(execBegin)+1:]
	end := strings.Index(rest, execEnd)
	if end < 0 {
		return nil, 0, false
	}
	payload := rest[:end]

	// Last, not first: a body that happens to contain the marker text must not
	// be able to truncate itself.
	marker := strings.LastIndex(payload, execStatus+"=")
	if marker < 0 {
		return nil, 0, false
	}
	if _, err := fmt.Sscanf(payload[marker:], execStatus+"=%d", &status); err != nil {
		return nil, 0, false
	}
	return []byte(strings.TrimSpace(payload[:marker])), status, true
}

// collapse squeezes a transcript onto one line for an error message.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// shortTaskID reduces an ECS task ARN to the id at its tail.
func shortTaskID(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}
