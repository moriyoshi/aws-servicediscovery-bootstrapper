// Package harness holds the plumbing both TiKV end-to-end suites need: running
// make and terraform with their output in the test log, reading terraform
// outputs, and retrying an assertion until a cluster settles.
//
// It is shared rather than duplicated because the two suites differ only in
// what they assert about, not in how they drive a stack up and wait for it.
// Nothing outside a test imports it.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// LineWriter forwards a child process's output to t.Log a line at a time, so
// terraform and docker progress stays readable under `go test -v`.
type LineWriter struct {
	T      *testing.T
	Prefix string
	buf    bytes.Buffer
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			w.buf.Reset()
			w.buf.WriteString(line) // keep the partial line for the next write
			break
		}
		w.T.Logf("%s%s", w.Prefix, strings.TrimRight(line, "\r\n"))
	}
	return len(p), nil
}

func (w *LineWriter) Flush() {
	if rest := strings.TrimSpace(w.buf.String()); rest != "" {
		w.T.Logf("%s%s", w.Prefix, rest)
	}
	w.buf.Reset()
}

// Run executes a command in the suite's own directory with its output tee'd
// into the test log. It deliberately has no timeout of its own: `go test
// -timeout` is the single place that bounds the run.
func Run(t *testing.T, name string, args ...string) error {
	t.Helper()
	t.Logf("$ %s %s", name, strings.Join(args, " "))

	out := &LineWriter{T: t, Prefix: "  | "}
	defer out.Flush()

	cmd := exec.Command(name, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = os.Environ()
	return cmd.Run()
}

func MakeTarget(t *testing.T, target string) error {
	t.Helper()
	return Run(t, "make", target)
}

// RequireTools skips the test when a tool the stack needs is missing, rather
// than failing halfway through provisioning.
func RequireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not on PATH; cannot provision the stack", name)
		}
	}
}

// --- terraform outputs -----------------------------------------------------

type TFOutputs map[string]struct {
	Value json.RawMessage `json:"value"`
}

func ReadTerraformOutputs(t *testing.T) TFOutputs {
	t.Helper()

	bin := os.Getenv("TERRAFORM")
	if bin == "" {
		bin = "terraform"
	}
	cmd := exec.Command(bin, "-chdir=terraform", "output", "-json")
	cmd.Stderr = &LineWriter{T: t, Prefix: "  | "}
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("terraform output: %v (has `make up` run?)", err)
	}

	var out TFOutputs
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse terraform output: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("terraform reported no outputs; the stack is not provisioned")
	}
	return out
}

func (o TFOutputs) Str(t *testing.T, key string) string {
	t.Helper()
	var v string
	o.Decode(t, key, &v)
	return v
}

func (o TFOutputs) Int(t *testing.T, key string) int {
	t.Helper()
	var v int
	o.Decode(t, key, &v)
	return v
}

func (o TFOutputs) Decode(t *testing.T, key string, into any) {
	t.Helper()
	entry, ok := o[key]
	if !ok {
		t.Fatalf("terraform output %q is missing", key)
	}
	if err := json.Unmarshal(entry.Value, into); err != nil {
		t.Fatalf("terraform output %q: %v", key, err)
	}
}

// --- waiting ---------------------------------------------------------------

// ErrTerminal marks a failure that retrying cannot fix, so Eventually gives up
// at once. Without it a dead cluster burns every subtest's full timeout in
// turn, which is how one broken run came to take 80 minutes to report a failure
// it knew about in 15.
var ErrTerminal = errors.New("not retryable")

// Eventually retries fn until it succeeds, hits a terminal error, or runs out
// of time, logging each failure so a timeout says what it was still waiting for.
func Eventually(t *testing.T, what string, timeout, interval time.Duration, fn func(context.Context) error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	var last error
	for attempt := 1; ; attempt++ {
		last = fn(ctx)
		if last == nil {
			t.Logf("%s: ok after %s", what, time.Since(start).Round(time.Second))
			return
		}
		if errors.Is(last, ErrTerminal) {
			t.Fatalf("%s: gave up after %s: %v", what, time.Since(start).Round(time.Second), last)
		}
		if ctx.Err() != nil {
			break
		}
		if attempt%5 == 1 {
			t.Logf("%s: waiting (%s elapsed): %v", what, time.Since(start).Round(time.Second), last)
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
	t.Fatalf("%s: timed out after %s: %v", what, timeout, last)
}
