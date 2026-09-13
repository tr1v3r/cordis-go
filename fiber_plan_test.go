package cordis

import "testing"

func planTestPlugin(name string, inject ...string) *Plugin[struct{}] {
	plugin := Define[struct{}](name, func(*Context, struct{}) error { return nil })
	return plugin.WithInject(inject...)
}

func loadPlanTestFiber(t *testing.T, root *Context, plugin *Plugin[struct{}]) *Fiber {
	t.Helper()
	fiber, err := root.Load(plugin, struct{}{})
	if err == nil {
		return fiber
	}
	t.Fatal(err)
	return nil
}

func setPlanTestState(f *Fiber, epoch string, cleaned, disposed, reload bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.epoch = epoch
	f.cleaned = cleaned
	f.disposed = disposed
	f.reloadRequested = reload
}

func TestPlanSelectsTransition(t *testing.T) {
	cases := []struct {
		name   string
		plugin *Plugin[struct{}]
		setup  func(*Fiber)
		want   fiberAction
	}{
		{
			name:   "disposed",
			plugin: planTestPlugin("disposed"),
			setup:  func(f *Fiber) { setPlanTestState(f, "", true, true, false) },
			want:   actionDispose,
		},
		{
			name:   "forced reload",
			plugin: planTestPlugin("reload", "missing"),
			setup:  func(f *Fiber) { setPlanTestState(f, epochInactive, true, false, true) },
			want:   actionReload,
		},
		{
			name:   "same epoch",
			plugin: planTestPlugin("same"),
			setup:  func(f *Fiber) { setPlanTestState(f, "", true, false, false) },
			want:   actionNoop,
		},
		{
			name:   "inactive epoch",
			plugin: planTestPlugin("unload", "missing"),
			setup:  func(f *Fiber) { setPlanTestState(f, "", true, false, false) },
			want:   actionUnload,
		},
		{
			name:   "clean load",
			plugin: planTestPlugin("load"),
			setup:  func(f *Fiber) { setPlanTestState(f, "old", true, false, false) },
			want:   actionLoad,
		},
		{
			name:   "dirty cycle",
			plugin: planTestPlugin("cycle"),
			setup:  func(f *Fiber) { setPlanTestState(f, "old", false, false, false) },
			want:   actionCycle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := New()
			fiber := loadPlanTestFiber(t, root, tc.plugin)
			tc.setup(fiber)

			got := fiber.plan()
			if got.action == tc.want {
				return
			}
			t.Fatalf("plan action = %d, want %d", got.action, tc.want)
		})
	}
}

func TestPlanConsumesReloadRequest(t *testing.T) {
	root := New()
	fiber := loadPlanTestFiber(t, root, planTestPlugin("consume"))
	setPlanTestState(fiber, "", true, false, true)

	fiber.plan()

	fiber.mu.Lock()
	reload := fiber.reloadRequested
	fiber.mu.Unlock()
	if reload {
		t.Fatal("reloadRequested was not consumed")
	}
}
