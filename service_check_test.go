package cordis_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/tr1v3r/cordis-go"
)

// checkPanicLogRecorder collects what the logger wrote, so a test can pin how
// often a broken availability probe is reported.
type checkPanicLogRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *checkPanicLogRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, strings.Split(strings.TrimSuffix(string(p), "\n"), "\n")...)
	return len(p), nil
}

func (r *checkPanicLogRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// TestAvailabilityCheckPanicIsLogged pins that a panicking availability
// predicate stays a diagnostic: the service counts as unavailable, the panic
// reaches the log, and a permanently broken probe is reported once instead of
// on every service resolution.
func TestAvailabilityCheckPanicIsLogged(t *testing.T) {
	recorder := &checkPanicLogRecorder{}
	root := cordis.New(cordis.WithWriter(recorder))
	if _, err := root.ProvideChecked("checked", &fakeDB{name: "checked"}, func() bool {
		panic("probe exploded")
	}); err != nil {
		t.Fatal(err)
	}
	// Every lookup checks the predicate more than once (the dependency
	// snapshot first, then the live registry); neither may report it again.
	for i := 0; i < 3; i++ {
		if _, ok := root.Get[*fakeDB]("checked"); ok {
			t.Fatal("a panicking availability check must leave the service unavailable")
		}
	}
	lines := recorder.snapshot()
	if len(lines) != 1 {
		t.Fatalf("want one availability check report, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "availability check") ||
		!strings.Contains(lines[0], "probe exploded") {
		t.Fatalf("want an availability check panic report, got %q", lines[0])
	}
}

// TestAvailabilityCheckPanicReportedOncePerBinding pins that the report latch
// lives on the binding, not on the service name: two isolated scopes can bind
// the same name, and each of their broken probes reports its own panic once
// while repeated lookups stay quiet.
func TestAvailabilityCheckPanicReportedOncePerBinding(t *testing.T) {
	recorder := &checkPanicLogRecorder{}
	root := cordis.New(cordis.WithWriter(recorder))
	left := root.Isolate("db")
	right := root.Isolate("db")
	for _, scoped := range []*cordis.Context{left, right} {
		if _, err := scoped.ProvideChecked("db", &fakeDB{name: "broken"},
			func() bool { panic("probe exploded") }); err != nil {
			t.Fatal(err)
		}
	}

	for _, scoped := range []*cordis.Context{left, right} {
		if _, ok := scoped.Get[*fakeDB]("db"); ok {
			t.Fatal("a panicking availability check must leave the service unavailable")
		}
	}
	if got := recorder.snapshot(); len(got) != 2 {
		t.Fatalf("want one report per binding, got %d: %q", len(got), got)
	}

	for _, scoped := range []*cordis.Context{left, right} {
		if _, ok := scoped.Get[*fakeDB]("db"); ok {
			t.Fatal("a panicking availability check must leave the service unavailable")
		}
	}
	if got := recorder.snapshot(); len(got) != 2 {
		t.Fatalf("want no report from repeated lookups, got %d: %q", len(got), got)
	}
}

// TestAvailabilityCheckWithoutPanicStaysSilent pins that the new reporting
// path only reacts to panics: a probe returning false hides the service, a
// probe returning true releases it, and neither writes a log line.
func TestAvailabilityCheckWithoutPanicStaysSilent(t *testing.T) {
	recorder := &checkPanicLogRecorder{}
	root := cordis.New(cordis.WithWriter(recorder))
	ready := false
	if _, err := root.ProvideChecked("db", &fakeDB{name: "late"},
		func() bool { return ready }); err != nil {
		t.Fatal(err)
	}

	if _, ok := root.Get[*fakeDB]("db"); ok {
		t.Fatal("unavailable service must not resolve")
	}
	ready = true
	if _, ok := root.Get[*fakeDB]("db"); !ok {
		t.Fatal("service must resolve once available")
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("want a non-panicking check to stay silent, got %d: %q", len(got), got)
	}
}
