package cordis_test

import (
	"fmt"
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

// TestScopedProvideDisposeRacingOwnerTeardownReleasesName covers the other half
// of the contract: registrations that belong to an effect scope while that effect
// is being torn down. Each worker owns an isolated scope under the effect, so the
// name it resolves can only ever be its own - with one shared scope and one name,
// a lookup after Dispose could legitimately observe a sibling's live registration
// and report a violation that never happened.
//
// The owner's unwind is allowed to run a child's disposer in its own goroutine;
// the child's Dispose must still not return before the name is released.
func TestScopedProvideDisposeRacingOwnerTeardownReleasesName(t *testing.T) {
	const (
		rounds  = 200
		workers = 8
	)
	for round := range rounds {
		root := cordis.New()
		scopeReady := make(chan *cordis.Context, 1)
		teardown := make(chan struct{})
		outerReady := make(chan cordis.Disposer, 1)
		go func() {
			outerReady <- root.Effect("outer", func(scope *cordis.Context) cordis.Disposer {
				scopeReady <- scope
				<-teardown
				return nil
			})
		}()
		effectScope := <-scopeReady

		scopes := make([]*cordis.Context, workers)
		for index := range scopes {
			scopes[index] = effectScope.IsolateShared("benchmark", "w"+strconv.Itoa(index))
		}

		var (
			hot      sync.WaitGroup
			done     sync.WaitGroup
			failures = make(chan string, workers)
		)
		hot.Add(workers)
		for index, scope := range scopes {
			done.Add(1)
			go func(worker int, scope *cordis.Context) {
				defer done.Done()
				first := true
				for {
					dispose, err := scope.Provide("benchmark", &fakeDB{name: "scoped"})
					if err != nil {
						// The scope expired; the registrations that did succeed
						// above are the ones this test judges.
						if first {
							hot.Done()
						}
						return
					}
					dispose()
					if _, ok := scope.Lookup("benchmark"); ok {
						select {
						case failures <- fmt.Sprintf(
							"worker %d: dispose returned but the service is still registered",
							worker):
						default:
						}
						return
					}
					if first {
						first = false
						hot.Done()
					}
				}
			}(index, scope)
		}
		hot.Wait()
		close(teardown)
		done.Wait()
		outer := <-outerReady
		outer()
		root.Fiber().Dispose()

		select {
		case message := <-failures:
			t.Fatalf("round %d: %s", round, message)
		default:
		}
	}
}
