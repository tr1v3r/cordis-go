package cordis_test

import (
	"strconv"
	"sync"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

// TestConcurrentProvideOnOneFiberReleasesTheName pins the fiber-level half of the
// concurrency contract: several goroutines providing and disposing their own
// isolated service on one shared fiber must have released the name by the time
// Dispose returns. Every scope is a Fork of the root, so none of them carries an
// explicit effect owner and nothing can be adopted across goroutines.
//
// This is the shape that used to fail: while ownership was inferred from a single
// per-fiber "current effect" marker, one goroutine's registration could be
// adopted by another goroutine's running effect, that effect's teardown ran the
// registration's disposer in its own goroutine, and the disposing goroutine saw
// its entry already claimed - returning with the name still registered, so its
// next Provide on that scope failed with SERVICE_EXISTS. It reproduced within a
// few thousand registrations at eight or more goroutines and never at two or four.
func TestConcurrentProvideOnOneFiberReleasesTheName(t *testing.T) {
	const (
		workers    = 12
		iterations = 3000
	)
	root := cordis.New()
	defer root.Fiber().Dispose()

	scopes := make([]*cordis.Context, workers)
	for index := range scopes {
		scopes[index] = root.IsolateShared("benchmark", "s"+strconv.Itoa(index))
	}

	var wg sync.WaitGroup
	for index, scope := range scopes {
		wg.Add(1)
		go func(worker int, scope *cordis.Context) {
			defer wg.Done()
			for iteration := range iterations {
				dispose, err := scope.Provide("benchmark", &fakeDB{name: "shared"})
				if err != nil {
					t.Errorf("worker %d iteration %d: provide: %v", worker, iteration, err)
					return
				}
				dispose()
				if _, ok := scope.Lookup("benchmark"); ok {
					t.Errorf("worker %d iteration %d: dispose returned but the service "+
						"is still registered", worker, iteration)
					return
				}
			}
		}(index, scope)
	}
	wg.Wait()
}
