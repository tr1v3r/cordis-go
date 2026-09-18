package cordis_test

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

func TestWaterfallFinalPanicIsContainedAtEveryTailPath(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*cordis.Context, string)
		run   func(*cordis.Context, string, func(string) any) any
	}{
		{
			name:  "no listeners",
			setup: func(*cordis.Context, string) {},
			run: func(root *cordis.Context, event string, final func(string) any) any {
				return root.Waterfall(event, "x", final)
			},
		},
		{
			name: "once listener already retired",
			setup: func(root *cordis.Context, event string) {
				root.OnWaterfall(event, func(value string, next func(string) any) any {
					return next(value)
				}, cordis.WithOnce())
				root.Waterfall(event, "retire", func(value string) any { return value })
			},
			run: func(root *cordis.Context, event string, final func(string) any) any {
				return root.Waterfall(event, "x", final)
			},
		},
		{
			name: "listener filtered by scope",
			setup: func(root *cordis.Context, event string) {
				right := root.Isolate("scope")
				right.OnWaterfall(event, func(value string, next func(string) any) any {
					return next(value)
				})
			},
			run: func(root *cordis.Context, event string, final func(string) any) any {
				left := root.Isolate("scope")
				return left.WaterfallScoped("scope", event, "x", final)
			},
		},
		{
			name: "listener panics before next",
			setup: func(root *cordis.Context, event string) {
				root.OnWaterfall(event, func(string, func(string) any) any {
					panic("listener exploded")
				})
			},
			run: func(root *cordis.Context, event string, final func(string) any) any {
				return root.Waterfall(event, "x", final)
			},
		},
		{
			name: "listener calls next",
			setup: func(root *cordis.Context, event string) {
				root.OnWaterfall(event, func(value string, next func(string) any) any {
					return next(value + "-next")
				})
			},
			run: func(root *cordis.Context, event string, final func(string) any) any {
				return root.Waterfall(event, "x", final)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &panicLogRecorder{}
			root := cordis.New(cordis.WithWriter(recorder))
			const event = "final/panic"
			test.setup(root, event)
			finals := 0

			var got any
			func() {
				defer func() {
					if reason := recover(); reason != nil {
						t.Errorf("want no panic to escape Waterfall, got %v", reason)
					}
				}()
				got = test.run(root, event, func(string) any {
					finals++
					panic("final exploded")
				})
			}()

			if got != nil {
				t.Fatalf("want nil after the final panic, got %v", got)
			}
			if finals != 1 {
				t.Fatalf("want 1 call to final, got %d", finals)
			}
			lines := recorder.snapshot()
			finalReports := 0
			for _, line := range lines {
				if strings.Contains(line, `event "final/panic" final panicked: final exploded`) {
					finalReports++
				}
				if strings.Contains(line, "listener panicked: final exploded") {
					t.Fatalf("want final panic attribution, got %q", line)
				}
			}
			if finalReports != 1 {
				t.Fatalf("want one final panic report, got %d: %q", finalReports, lines)
			}
		})
	}
}

func TestWaterfallConcurrentNextCallsShareOneInProgressStep(t *testing.T) {
	root := cordis.New()
	const callers = 16
	var listenerCalls atomic.Int32
	var finalCalls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	results := make(chan any, callers)

	root.OnWaterfall("concurrent", func(value string, next func(string) any) any {
		var ready sync.WaitGroup
		ready.Add(callers)
		start := make(chan struct{})
		for range callers {
			go func() {
				ready.Done()
				<-start
				results <- next(value + "-shared")
			}()
		}
		ready.Wait()
		close(start)
		var first any
		for i := 0; i < callers; i++ {
			result := <-results
			if i == 0 {
				first = result
			}
			if result != "final:x-shared-listener" {
				t.Errorf("want final:x-shared-listener, got %v", result)
			}
		}
		return first
	})
	root.OnWaterfall("concurrent", func(value string, next func(string) any) any {
		if listenerCalls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return next(value + "-listener")
	})

	waterfallResult := make(chan any, 1)
	go func() {
		waterfallResult <- root.Waterfall("concurrent", "x", func(value string) any {
			finalCalls.Add(1)
			return "final:" + value
		})
	}()
	<-entered
	close(release)

	if got := <-waterfallResult; got != "final:x-shared-listener" {
		t.Fatalf("want final:x-shared-listener, got %v", got)
	}
	if got := listenerCalls.Load(); got != 1 {
		t.Fatalf("want 1 invocation of the claimed listener step, got %d", got)
	}
	if got := finalCalls.Load(); got != 1 {
		t.Fatalf("want 1 invocation of final, got %d", got)
	}
}

func TestWaterfallSavedNextCanResumeAfterReturn(t *testing.T) {
	root := cordis.New()
	var savedNext func(string) any
	var finalCalls atomic.Int32
	root.OnWaterfall("saved", func(_ string, next func(string) any) any {
		savedNext = next
		return "deferred"
	})

	got := root.Waterfall("saved", "x", func(value string) any {
		finalCalls.Add(1)
		return "final:" + value
	})
	if got != "deferred" {
		t.Fatalf("want deferred listener result, got %v", got)
	}
	if got := finalCalls.Load(); got != 0 {
		t.Fatalf("want 0 final calls before the saved continuation, got %d", got)
	}
	if got := savedNext("x-late"); got != "final:x-late" {
		t.Fatalf("want final:x-late from the saved continuation, got %v", got)
	}
	if got := savedNext("x-again"); got != "final:x-late" {
		t.Fatalf("want settled result final:x-late, got %v", got)
	}
	if got := finalCalls.Load(); got != 1 {
		t.Fatalf("want 1 invocation of final, got %d", got)
	}
}
