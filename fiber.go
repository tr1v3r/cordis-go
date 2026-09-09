package cordis

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// FiberState is the lifecycle state of one plugin instance.
//
// Pending waits for required services; Loading runs the plugin body; Active is
// loaded and providing; Failed means the body or its config returned an error;
// Unloading runs disposers; Disposed cannot restart.
type FiberState int

// Fiber lifecycle states.
const (
	StatePending FiberState = iota
	StateLoading
	StateActive
	StateFailed
	StateUnloading
	StateDisposed
)

func (s FiberState) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateLoading:
		return "loading"
	case StateActive:
		return "active"
	case StateFailed:
		return "failed"
	case StateUnloading:
		return "unloading"
	case StateDisposed:
		return "disposed"
	default:
		return "unknown"
	}
}

// epochInactive is the sentinel epoch of a fiber whose dependencies are unmet.
// A satisfiable epoch is either "" (no dependencies) or ":"-prefixed provider
// uids, so the two can never collide.
const epochInactive = "\x00inactive"

// Fiber is one running instance of a plugin.
//
// A fiber owns a context, the effects registered through it, and the services
// it provides. Loading the same plugin twice creates two independent fibers
// under one shared runtime, exactly like Cordis v4.
type Fiber struct {
	// UID is unique within the application; 0 is the root fiber.
	UID int
	// Parent is the context the plugin was loaded from.
	Parent *Context
	// Ctx is the fiber's own context, passed to the plugin body.
	Ctx *Context

	runtime *runtime
	inject  map[string]struct{}

	mu        sync.Mutex
	state     FiberState
	err       error
	epoch     string
	store     map[string]*impl
	config    any
	rawConfig any
	effects   *disposableList
	busy      bool
	dirty     bool
	disposed  bool
	cleaned   bool
	root      bool
	current   *effectEntry

	done           chan struct{}
	goctx          context.Context
	cancel         context.CancelFunc
	parentDisposer Disposer
}

// StatusEvent is emitted as "internal/status" whenever a fiber changes state.
type StatusEvent struct {
	Fiber *Fiber
	Old   FiberState
}

// PluginEvent is emitted as "internal/plugin" when a fiber is created or
// disposed.
type PluginEvent struct {
	Fiber *Fiber
}

func newRootFiber(ctx *Context) *Fiber {
	goctx, cancel := context.WithCancel(context.Background())
	return &Fiber{
		UID:     0,
		Ctx:     ctx,
		state:   StateActive,
		root:    true,
		store:   map[string]*impl{},
		effects: newDisposableList(),
		done:    make(chan struct{}),
		goctx:   goctx,
		cancel:  cancel,
	}
}

func newFiber(parent *Context, rt *runtime, cfg any, inject map[string]struct{}) *Fiber {
	goctx, cancel := context.WithCancel(parent.fiber.goctx)
	fiber := &Fiber{
		Parent:    parent,
		runtime:   rt,
		inject:    inject,
		rawConfig: cfg,
		state:     StatePending,
		epoch:     epochInactive,
		effects:   newDisposableList(),
		done:      make(chan struct{}),
		goctx:     goctx,
		cancel:    cancel,
	}
	fiber.UID = parent.shared.nextUID()
	fiber.Ctx = parent.Fork(rt.name)
	fiber.Ctx.fiber = fiber

	// The parent owns the child's lifetime: disposing the parent disposes every
	// plugin loaded beneath it.
	fiber.parentDisposer = parent.fiber.onDispose(fiber.Dispose)
	parent.shared.addFiber(rt, fiber)
	parent.shared.bus.emitInternal("internal/plugin", &PluginEvent{Fiber: fiber})
	fiber.refresh()
	return fiber
}

func (f *Fiber) shared() *core { return f.Ctx.shared }

// State returns the current lifecycle state.
func (f *Fiber) State() FiberState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// Error returns the error that failed the last load, if any.
func (f *Fiber) Error() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Config returns the config the plugin was last activated with.
func (f *Fiber) Config() any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.config
}

// RawConfig returns the config as supplied, before validation.
func (f *Fiber) RawConfig() any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rawConfig
}

// Name returns the plugin name, inherited from the nearest named ancestor.
func (f *Fiber) Name() string {
	if f.runtime != nil && f.runtime.name != "" {
		return f.runtime.name
	}
	if f.Parent == nil || f.root {
		return "root"
	}
	return f.Parent.fiber.Name()
}

// Inject returns the required service names, sorted.
func (f *Fiber) Inject() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.inject))
	for name := range f.inject {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Store returns the service implementations this fiber resolved while active.
func (f *Fiber) Store() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]any, len(f.store))
	for name, impl := range f.store {
		out[name] = impl.value
	}
	return out
}

// Effects returns the live effect metadata tree of this fiber.
func (f *Fiber) Effects() []*EffectMeta {
	entries := f.effects.snapshot()
	out := make([]*EffectMeta, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.meta)
	}
	return out
}

// onDispose registers a raw disposer on the fiber.
func (f *Fiber) onDispose(fn func()) Disposer {
	return f.effect("anonymous", func() Disposer { return fn })
}

// effect registers a reversible side effect, mirroring Fiber#effect(). It
// panics with INACTIVE_EFFECT when the fiber is gone, like Cordis does.
func (f *Fiber) effect(label string, body func() Disposer) Disposer {
	disposer, err := f.tryEffect(label, body)
	if err != nil {
		panic(err)
	}
	return disposer
}

// tryEffect is effect with an error result, for callers that must not panic.
//
// The entry is published before the body runs. If the body disposes the fiber,
// or another goroutine unloads it while the body runs, the teardown is deferred
// to this function instead of being lost.
func (f *Fiber) tryEffect(label string, body func() Disposer) (Disposer, error) {
	f.mu.Lock()
	if f.disposed || f.state == StateUnloading {
		f.mu.Unlock()
		return nil, newError(ErrInactiveEffect, "cannot create effect on inactive context %q", f.Name())
	}
	entry := &effectEntry{meta: &EffectMeta{Label: label}}
	parent := f.current
	var remove func() bool
	if parent != nil {
		// A nested effect is owned by the enclosing one, exactly like Cordis's
		// effect collector: disposing the outer effect disposes its children.
		parent.addChild(entry)
	} else {
		_, remove = f.effects.add(entry)
	}
	f.current = entry
	f.mu.Unlock()

	dispose, panicked := runEffectBody(body)

	f.mu.Lock()
	f.current = parent
	f.mu.Unlock()

	if panicked != nil {
		if parent != nil {
			parent.detach(entry)
		} else {
			remove()
		}
		entry.abandon()
		panic(panicked)
	}
	if dispose == nil {
		dispose = func() {}
	}
	if entry.setDispose(dispose) {
		// The owning fiber unloaded while the body ran; unwind immediately.
		f.runDisposer(entry)
		return func() {}, nil
	}
	return Once(func() {
		if parent != nil {
			parent.detach(entry)
		} else {
			remove()
		}
		f.runDisposer(entry)
	}), nil
}

func runEffectBody(body func() Disposer) (dispose Disposer, panicked any) {
	defer func() { panicked = recover() }()
	return body(), nil
}

func (f *Fiber) runDisposer(entry *effectEntry) {
	dispose, children := entry.take()
	if dispose == nil && children == nil {
		// Already unwound, or still being set up by its own body.
		return
	}
	if dispose != nil {
		f.callDisposer(entry, dispose)
	}
	// Nested effects unwind after their owner, newest first.
	for i := len(children) - 1; i >= 0; i-- {
		f.runDisposer(children[i])
	}
}

func (f *Fiber) callDisposer(entry *effectEntry, dispose Disposer) {
	defer func() {
		if reason := recover(); reason != nil {
			f.shared().log.errorf("cordis: panic while disposing %s of plugin %s: %v", entry.meta.Label, f.Name(), reason)
		}
	}()
	dispose()
}

// refresh recomputes the dependency epoch and drives a load/unload transition.
//
// It is re-entrant safe: nested refresh requests set a dirty flag that the
// outermost call drains, so a plugin that provides a service another plugin
// waits for cannot recurse without bound.
func (f *Fiber) refresh() {
	f.mu.Lock()
	if f.disposed {
		busy := f.busy
		f.mu.Unlock()
		if !busy {
			// Nothing is transitioning: finish the disposal teardown here.
			f.sync()
		}
		return
	}
	if f.busy {
		f.dirty = true
		f.mu.Unlock()
		return
	}
	f.busy = true
	f.mu.Unlock()

	for {
		f.mu.Lock()
		f.dirty = false
		f.mu.Unlock()

		f.sync()

		// Re-check under the same lock that clears busy: Dispose sets dirty
		// under this lock, so either the loop sees it and unwinds, or it sees
		// busy==false and performs the teardown itself. Both orders are safe.
		f.mu.Lock()
		if !f.dirty {
			f.busy = false
			f.mu.Unlock()
			break
		}
		f.mu.Unlock()
	}
}

func (f *Fiber) sync() {
	if f.root || f.runtime == nil {
		return
	}

	f.mu.Lock()
	disposed := f.disposed
	f.mu.Unlock()
	if disposed {
		f.unload()
		return
	}

	epoch := f.computeEpoch()

	f.mu.Lock()
	if f.disposed || epoch == f.epoch {
		f.mu.Unlock()
		return
	}
	f.epoch = epoch
	f.mu.Unlock()

	if epoch == epochInactive {
		f.unload()
		return
	}
	f.load()
}

// computeEpoch captures the identity of every provider this fiber depends on.
// When a provider is replaced, the epoch changes and the fiber reloads.
func (f *Fiber) computeEpoch() string {
	names := f.Inject()
	var builder strings.Builder
	for _, name := range names {
		impl := f.shared().lookupStrict(name, f.Ctx.isolateLabel(name))
		if impl == nil {
			return epochInactive
		}
		builder.WriteByte(':')
		builder.WriteString(strconv.Itoa(impl.fiber.UID))
	}
	return builder.String()
}

func (f *Fiber) load() {
	f.mu.Lock()
	f.cleaned = false
	f.mu.Unlock()
	f.setState(StateLoading)

	store := make(map[string]*impl, len(f.inject))
	for _, name := range f.Inject() {
		impl := f.shared().lookupStrict(name, f.Ctx.isolateLabel(name))
		if impl == nil {
			// A dependency vanished between the epoch computation and now.
			f.refresh()
			return
		}
		store[name] = impl
	}

	f.mu.Lock()
	f.store = store
	raw := f.rawConfig
	f.mu.Unlock()

	config, err := f.runtime.def.ResolveConfig(raw)
	if err != nil {
		f.fail(err)
		return
	}
	f.mu.Lock()
	f.config = config
	f.mu.Unlock()

	if err := f.run(config); err != nil {
		f.fail(err)
		return
	}
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	f.setState(StateActive)
}

func (f *Fiber) run(config any) (err error) {
	defer func() {
		if reason := recover(); reason != nil {
			err = fmt.Errorf("panic in plugin %s: %v", f.Name(), reason)
		}
	}()
	return f.runtime.def.Run(f.Ctx, config)
}

func (f *Fiber) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
	f.setState(StateFailed)
	f.shared().log.errorf("cordis: plugin %s failed to load: %v", f.Name(), err)
}

func (f *Fiber) unload() {
	f.mu.Lock()
	if f.cleaned {
		f.mu.Unlock()
		return
	}
	f.cleaned = true
	f.mu.Unlock()

	f.setState(StateUnloading)
	for _, entry := range f.effects.clear() {
		f.runDisposer(entry)
	}
	f.mu.Lock()
	f.store = nil
	f.config = nil
	disposed := f.disposed
	f.mu.Unlock()
	if disposed {
		f.setState(StateDisposed)
		return
	}
	f.setState(StatePending)
}

func (f *Fiber) setState(state FiberState) {
	f.mu.Lock()
	old := f.state
	f.state = state
	f.mu.Unlock()
	if old == state {
		return
	}
	f.shared().bus.emitInternal("internal/status", &StatusEvent{Fiber: f, Old: old})

	// Only a change of service availability wakes dependents.
	if (old == StateActive) != (state == StateActive) {
		f.notifyProvided()
	}
}

// notifyProvided re-evaluates every fiber that injects a service this fiber
// owns, which is what makes dependency replacement propagate.
func (f *Fiber) notifyProvided() {
	for _, name := range f.shared().providedNames(f) {
		f.shared().notify(name, f.Ctx.isolateLabel(name))
	}
}

// Dispose unloads the plugin and releases everything it registered. It is
// idempotent.
func (f *Fiber) Dispose() {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		return
	}
	f.disposed = true
	busy := f.busy
	f.mu.Unlock()

	// Stop dependent goroutines immediately, then unwind effects.
	f.cancel()
	close(f.done)

	if f.runtime != nil {
		f.shared().removeFiber(f.runtime, f)
		f.shared().bus.emitInternal("internal/plugin", &PluginEvent{Fiber: f})
	}

	if busy {
		// A transition is in flight. Hand the teardown to it instead of
		// unloading concurrently with the plugin body that is still running;
		// the loop in refresh() observes the flag and unwinds.
		f.mu.Lock()
		f.dirty = true
		f.mu.Unlock()
		return
	}
	f.unload()
	f.setState(StateDisposed)

	// Release the slot this child occupied in the parent's effect list, so a
	// parent that repeatedly loads and disposes plugins does not grow forever.
	if f.parentDisposer != nil {
		f.parentDisposer()
		f.parentDisposer = nil
	}
}

// Restart unloads and reloads the plugin with its current config.
func (f *Fiber) Restart() error {
	if err := f.assertActive(); err != nil {
		return err
	}
	f.unload()
	f.mu.Lock()
	f.epoch = epochInactive
	f.err = nil
	f.mu.Unlock()
	f.refresh()
	return f.Error()
}

// Update replaces the raw config and restarts the plugin.
//
// A pending or failed fiber is unloaded and forced through a fresh load, because
// its epoch may already equal the computed one and a plain refresh would then
// no-op while silently discarding the previous error.
func (f *Fiber) Update(config any) error {
	if err := f.assertActive(); err != nil {
		return err
	}
	f.mu.Lock()
	f.rawConfig = config
	f.err = nil
	f.mu.Unlock()

	if f.State() != StateActive {
		f.unload()
		f.mu.Lock()
		f.epoch = epochInactive
		f.mu.Unlock()
		f.refresh()
		return f.Error()
	}
	return f.Restart()
}

func (f *Fiber) assertActive() error {
	f.mu.Lock()
	disposed := f.disposed
	f.mu.Unlock()
	if disposed {
		return newError(ErrInactiveEffect, "cannot operate on disposed fiber %q", f.Name())
	}
	return nil
}

func (f *Fiber) injects(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.inject[name]
	return ok
}

func (c *core) nextUID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counter++
	return c.counter
}

func (c *core) addFiber(rt *runtime, fiber *Fiber) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rt.fibers = append(rt.fibers, fiber)
}

func (c *core) removeFiber(rt *runtime, fiber *Fiber) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, candidate := range rt.fibers {
		if candidate == fiber {
			rt.fibers = append(rt.fibers[:i], rt.fibers[i+1:]...)
			break
		}
	}
	if len(rt.fibers) == 0 {
		delete(c.plugins, rt.def)
	}
}

// snapshotFibers returns a copy of every live fiber.
func (c *core) snapshotFibers() []*Fiber {
	c.mu.Lock()
	defer c.mu.Unlock()
	var fibers []*Fiber
	for _, rt := range c.plugins {
		fibers = append(fibers, rt.fibers...)
	}
	return fibers
}
