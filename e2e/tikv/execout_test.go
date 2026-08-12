package tikv

import "testing"

// A real transcript: the plugin's banner, then the fenced payload, then the
// closing line, with the pty's CRLFs intact.
const sessionTranscript = "\r\nThe Session Manager plugin was installed successfully.\r\n\r\n" +
	"Starting session with SessionId: ecs-execute-command-0123456789abcdef0\r\n" +
	"MUSTER_E2E_BEGIN\r\n" +
	"{\"id\":7449028461762886431,\"max_peer_count\":3}\r\n" +
	"MUSTER_E2E_STATUS=0\r\n" +
	"MUSTER_E2E_END\r\n\r\n" +
	"Exiting session with sessionId: ecs-execute-command-0123456789abcdef0.\r\n"

func TestParseExecOutput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         string
		wantBody   string
		wantStatus int
		wantOK     bool
	}{
		{
			name:     "session transcript",
			in:       sessionTranscript,
			wantBody: `{"id":7449028461762886431,"max_peer_count":3}`,
			wantOK:   true,
		},
		{
			// curl -sS -f writes its diagnostic to stderr, which the pty
			// interleaves into the same stream. Status is what matters.
			name:       "curl failure",
			in:         "MUSTER_E2E_BEGIN\ncurl: (22) The requested URL returned error: 500\n\nMUSTER_E2E_STATUS=22\nMUSTER_E2E_END\n",
			wantBody:   "curl: (22) The requested URL returned error: 500",
			wantStatus: 22,
			wantOK:     true,
		},
		{
			name:     "empty body",
			in:       "MUSTER_E2E_BEGIN\n\nMUSTER_E2E_STATUS=0\nMUSTER_E2E_END\n",
			wantBody: "",
			wantOK:   true,
		},
		{
			// PD serves regions as one long line; nothing may be inserted into it.
			name:     "single long line",
			in:       "MUSTER_E2E_BEGIN\r\n" + longJSON + "\r\nMUSTER_E2E_STATUS=0\r\nMUSTER_E2E_END\r\n",
			wantBody: longJSON,
			wantOK:   true,
		},
		{
			// A body echoing the marker must not be able to truncate itself:
			// the parser takes the last occurrence, which is the real one.
			name:     "marker text inside the body",
			in:       "MUSTER_E2E_BEGIN\n{\"note\":\"MUSTER_E2E_STATUS=99\"}\nMUSTER_E2E_STATUS=0\nMUSTER_E2E_END\n",
			wantBody: `{"note":"MUSTER_E2E_STATUS=99"}`,
			wantOK:   true,
		},
		{
			// The session died before the command ran: retryable, not a failure
			// of the thing under test.
			name:   "no fence",
			in:     "An error occurred (TargetNotConnectedException) when calling the ExecuteCommand operation\n",
			wantOK: false,
		},
		{
			name:   "truncated session",
			in:     "MUSTER_E2E_BEGIN\n{\"id\":1}\n",
			wantOK: false,
		},
		{
			name:   "fence without status",
			in:     "MUSTER_E2E_BEGIN\n{\"id\":1}\nMUSTER_E2E_END\n",
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, status, ok := parseExecOutput(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if string(body) != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

var longJSON = `{"count":1,"regions":[` + func() string {
	s := ""
	for i := 0; i < 200; i++ {
		s += `{"id":1,"peers":[{"id":2,"store_id":3}]},`
	}
	return s[:len(s)-1]
}() + `]}`

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

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abc", 3); got != "abc" {
		t.Errorf("truncate should leave a short string alone, got %q", got)
	}
}
