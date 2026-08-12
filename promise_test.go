package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.starlark.net/starlark"
)

// syncBuffer is an io.Writer a slog handler can share with the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A rejected promise is inert: it holds its error until someone joins it, and if
// nobody ever does, the error is gone. That is correct for a probe raced by
// any_true() and catastrophic for a background task, which is how a reporting
// loop in the Google Cloud end-to-end stack died on its first iteration in total
// silence -- see TestE2ETiKVGCPReportSurvivesADeadPD.
func TestUnobservedTaskFailureIsReported(t *testing.T) {
	newEngine := func(t *testing.T) (*engine, *syncBuffer) {
		t.Helper()
		out := &syncBuffer{}
		deps := &engineDeps{logger: slog.New(slog.NewTextHandler(out, nil))}
		eng, err := loadScript(context.Background(), writeScript(t, "def main():\n    return go(lambda: 1)\n"), deps)
		if err != nil {
			t.Fatalf("loadScript: %v", err)
		}
		eng.unobservedGrace = 20 * time.Millisecond
		return eng, out
	}

	boom := func(context.Context) (starlark.Value, error) { return nil, errors.New("boom") }

	t.Run("nobody joins", func(t *testing.T) {
		eng, out := newEngine(t)
		p := eng.spawnFunc(context.Background(), "reporter", boom, false)
		waitSettled(t, p)
		time.Sleep(200 * time.Millisecond)

		got := out.String()
		if !strings.Contains(got, "boom") {
			t.Fatalf("a task failed with nothing to join it and nothing was logged; got %q", got)
		}
		if !strings.Contains(got, "reporter") {
			t.Errorf("the report does not name the task, so it cannot be traced back: %q", got)
		}
	})

	t.Run("joined", func(t *testing.T) {
		eng, out := newEngine(t)
		p := eng.spawnFunc(context.Background(), "reporter", boom, false)
		if _, err := p.await(context.Background()); err == nil {
			t.Fatal("await should surface the rejection")
		}
		time.Sleep(200 * time.Millisecond)

		// The joiner already has the error. Reporting it here as well would put
		// two lines in the log for every ordinary failure.
		if got := out.String(); got != "" {
			t.Errorf("a joined failure was reported again by the engine: %q", got)
		}
	})

	t.Run("cancelled is not a failure", func(t *testing.T) {
		eng, out := newEngine(t)
		p := eng.spawnFunc(context.Background(), "reporter", func(ctx context.Context) (starlark.Value, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, false)
		p.doCancel()
		waitSettled(t, p)
		time.Sleep(200 * time.Millisecond)

		if got := out.String(); got != "" {
			t.Errorf("an intentional cancel was reported as a lost failure: %q", got)
		}
	})
}

// select() and any_true() read doneCh directly rather than through await(), so
// they have to record the observation themselves or every promise they race
// would be reported as an unobserved failure.
func TestSelectCountsAsObserving(t *testing.T) {
	out := &syncBuffer{}
	deps := &engineDeps{logger: slog.New(slog.NewTextHandler(out, nil))}
	eng, err := loadScript(context.Background(), writeScript(t,
		"def boom():\n    fail('boom')\n\ndef main():\n    return select(go(boom))\n"), deps)
	if err != nil {
		t.Fatalf("loadScript: %v", err)
	}
	eng.unobservedGrace = 20 * time.Millisecond

	if _, err := eng.callMain(context.Background()); err != nil {
		t.Fatalf("select() must not raise on a rejected promise: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := out.String(); got != "" {
		t.Errorf("select() awaited the promise, but the failure was reported as unobserved: %q", got)
	}
}

func waitSettled(t *testing.T, p *promise) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if p.isSettled() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("promise never settled")
}
