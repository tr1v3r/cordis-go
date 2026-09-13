package cordis_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/tr1v3r/cordis-go"
)

// panicLogRecorder counts what the application logger writes, so a test can pin
// how many reports one dispatch produced.
type panicLogRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *panicLogRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, strings.Split(strings.TrimSuffix(string(p), "\n"), "\n")...)
	return len(p), nil
}

func (r *panicLogRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func wantSinglePanicLogLine(t *testing.T, recorder *panicLogRecorder) {
	t.Helper()
	lines := recorder.snapshot()
	if len(lines) != 1 {
		t.Fatalf("want one panic log line, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "listener panicked") || !strings.Contains(lines[0], "boom") {
		t.Fatalf("want a listener panic report, got %q", lines[0])
	}
}

// TestListenerPanicLoggedOncePerDispatchPath pins that a panicking listener is
// reported exactly once, whichever dispatch path invoked it: the report is the
// only diagnostic a host gets, so a duplicate doubles every panic.
func TestListenerPanicLoggedOncePerDispatchPath(t *testing.T) {
	t.Run("emit", func(t *testing.T) {
		recorder := &panicLogRecorder{}
		root := cordis.New(cordis.WithWriter(recorder))
		root.On("panic/emit", func(int) { panic("boom") })
		root.Emit("panic/emit", 1)
		wantSinglePanicLogLine(t, recorder)
	})

	t.Run("bail", func(t *testing.T) {
		recorder := &panicLogRecorder{}
		root := cordis.New(cordis.WithWriter(recorder))
		root.On("panic/bail", func(int) { panic("boom") })
		if _, bailed := root.Bail("panic/bail", 1); bailed {
			t.Fatal("want no bail, got one")
		}
		wantSinglePanicLogLine(t, recorder)
	})

	t.Run("parallel", func(t *testing.T) {
		recorder := &panicLogRecorder{}
		root := cordis.New(cordis.WithWriter(recorder))
		root.On("panic/parallel", func(int) { panic("boom") })
		if err := root.Parallel("panic/parallel", 1); err == nil {
			t.Fatal("want the panic reported to the caller, got nil")
		}
		wantSinglePanicLogLine(t, recorder)
	})

	t.Run("waterfall", func(t *testing.T) {
		recorder := &panicLogRecorder{}
		root := cordis.New(cordis.WithWriter(recorder))
		root.OnWaterfall("panic/waterfall", func(_ int, _ func(int) any) any { panic("boom") })
		root.Waterfall("panic/waterfall", 1, func(int) any { return nil })
		wantSinglePanicLogLine(t, recorder)
	})
}
