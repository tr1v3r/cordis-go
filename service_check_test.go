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
