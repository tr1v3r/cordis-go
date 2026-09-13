package cordis_test

import (
	"errors"
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

// TestInternalPluginEventPanicLoggedOncePerDispatch pins the internal event
// stream, which is the one dispatch path no host dispatch reaches: creating a
// fiber and starting its disposal each emit "internal/plugin", and each of
// those dispatches must report a broken listener exactly once.
func TestInternalPluginEventPanicLoggedOncePerDispatch(t *testing.T) {
	recorder := &panicLogRecorder{}
	root := cordis.New(cordis.WithWriter(recorder))
	root.On("internal/plugin", func(*cordis.PluginEvent) { panic("boom") })
	plugin := cordis.Define[struct{}]("p", func(_ *cordis.Context, _ struct{}) error {
		return nil
	})

	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	wantSinglePanicLogLine(t, recorder)

	fiber.Dispose()
	if got := len(recorder.snapshot()); got != 2 {
		t.Fatalf("want one report per internal dispatch, got %d: %q",
			got, recorder.snapshot())
	}
}

// TestPanicReportNamesEventAndValue pins what the single report says: the host
// can only route the failure if the report carries the event name and the
// original panic value, including non-string values like errors.
func TestPanicReportNamesEventAndValue(t *testing.T) {
	recorder := &panicLogRecorder{}
	root := cordis.New(cordis.WithWriter(recorder))
	root.On("panic/value", func(int) { panic(errors.New("disk full")) })

	root.Emit("panic/value", 1)

	lines := recorder.snapshot()
	if len(lines) != 1 {
		t.Fatalf("want one panic log line, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], `event "panic/value"`) {
		t.Fatalf("want the event name in the report, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "disk full") {
		t.Fatalf("want the panic value in the report, got %q", lines[0])
	}
}

// TestPanickingOnceListenerReportedOnceAndRetired pins that the once-per-
// dispatch report coexists with once-listener retirement: the broken listener
// is reported for the dispatch that ran it and never again afterwards.
func TestPanickingOnceListenerReportedOnceAndRetired(t *testing.T) {
	recorder := &panicLogRecorder{}
	root := cordis.New(cordis.WithWriter(recorder))
	fired := 0
	root.OnOnce("panic/once", func(int) { fired++; panic("boom") })

	root.Emit("panic/once", 1)
	if fired != 1 {
		t.Fatalf("want 1 invocation of the panicking once listener, got %d", fired)
	}
	wantSinglePanicLogLine(t, recorder)

	root.Emit("panic/once", 2)
	if fired != 1 {
		t.Fatalf("want 1 invocation after the retry dispatch, got %d", fired)
	}
	if got := len(recorder.snapshot()); got != 1 {
		t.Fatalf("want no report from the retired listener, got %d: %q",
			got, recorder.snapshot())
	}
}
