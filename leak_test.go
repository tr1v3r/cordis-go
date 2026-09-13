package cordis_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package when a test leaves a goroutine behind. The
// lifecycle machinery is the heart of this library: a dispose path that strands
// a loader goroutine, a watcher or a dispatcher is a leak even when every
// assertion in the suite passes, so the whole package is checked once, after
// the last test returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
