package loader_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package when a test leaves a goroutine behind: tree loads
// drive the core fiber machinery, and a rollback that strands a loader
// goroutine would only show up here, after the last test returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
