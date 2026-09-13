package cordis

import (
	"context"
	"fmt"
	"slices"
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

// String returns the state's lowercase name.
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
// under one shared plugin runtime, exactly like Cordis v4.
type Fiber struct {
	// UID is unique within the application; 0 is the root fiber.
	UID int
	// Parent is the context the plugin was loaded from.
	Parent *Context
	// Ctx is the fiber's own context, passed to the plugin body.
	Ctx *Context

	runtime *runtime
	inject  map[string]struct{}

	mu               sync.Mutex
	state            FiberState
	err              error
	epoch            string
	resolvedServices map[string]*serviceBinding
	config           any
	rawConfig        any
	effects          *disposableList
	busy             bool
	dirty            bool
	forceReload      bool
	disposed         bool
	hasEffects       bool
	root             bool
	current          *effectEntry

	done                 chan struct{}
	disposedDone         chan struct{}
	lifecycleCtx         context.Context
	cancel               context.CancelFunc
	parentEffectDisposer Disposer
	disposeOnce          sync.Once
}

// StatusEvent is emitted as "internal/status" whenever a fiber changes state.
type StatusEvent struct {
	Fiber *Fiber
	Old   FiberState
}

// PluginEvent is emitted as "internal/plugin" when a fiber is created and when
// its disposal starts, before effects are unwound.
type PluginEvent struct {
	Fiber *Fiber
}

func newRootFiber(ctx *Context) *Fiber {
	// The root lifecycle context descends from the host context, when there is
	// one, so plugins inherit its values and deadlines the same way they inherit
	// those of a parent fiber.
	base := ctx.shared.baseCtx
	if base == nil {
		base = context.Background()
	}
	lifecycleCtx, cancel := context.WithCancel(base)
	return &Fiber{
		UID:              0,
		Ctx:              ctx,
		state:            StateActive,
		root:             true,
		resolvedServices: map[string]*serviceBinding{},
		effects:          newDisposableList(),
		hasEffects:       true,
		done:             make(chan struct{}),
		disposedDone:     make(chan struct{}),
		lifecycleCtx:     lifecycleCtx,
		cancel:           cancel,
	}
}

func newFiber(parentCtx *Context, runtime *runtime, cfg any,
	inject map[string]struct{}) *Fiber {
	lifecycleCtx, cancel := context.WithCancel(parentCtx.fiber.lifecycleCtx)
	fiber := &Fiber{
		Parent:       parentCtx,
		runtime:      runtime,
		inject:       inject,
		rawConfig:    cfg,
		state:        StatePending,
		epoch:        epochInactive,
		effects:      newDisposableList(),
		hasEffects:   false,
		done:         make(chan struct{}),
		disposedDone: make(chan struct{}),
		lifecycleCtx: lifecycleCtx,
		cancel:       cancel,
	}
	fiber.UID = parentCtx.shared.nextUID()
	fiber.Ctx = parentCtx.Fork(runtime.name)
	fiber.Ctx.fiber = fiber
	return fiber
}

// start attaches the fiber to its parent and publishes it. It returns an error
// when the parent is already unloading or disposed, so the caller must abort
// instead of leaving an orphan fiber behind.
func (f *Fiber) start() error {
	parent := f.Parent

	// The parent owns the child lifetime: disposing the parent disposes every
	// plugin loaded beneath it. Use tryEffect so a parent that starts unloading
	// during Load returns an error instead of panicking out of the constructor.
	parentEffectDisposer, err := parent.fiber.tryEffect("child",
		func() Disposer { return f.Dispose })
	if err != nil {
		return err
	}
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		parentEffectDisposer()
	} else {
		f.parentEffectDisposer = parentEffectDisposer
		f.mu.Unlock()
	}
	f.shared().addFiber(f.runtime, f)
	f.shared().bus.emitInternal("internal/plugin", &PluginEvent{Fiber: f})
	f.refresh()
	return nil
}

func (f *Fiber) shared() *core { return f.Ctx.shared }

// State returns the current lifecycle state.
func (f *Fiber) State() FiberState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// Disposed returns a channel closed once this fiber reaches StateDisposed.
//
// It is a barrier for the terminal state of this fiber, not for every resource
// in its subtree. An effect body that was still initializing when teardown began
// may run its disposer after Disposed closes, and a busy child fiber may finish
// its own cleanup later. Use it to wait for this fiber to reach its terminal
// state, not as a whole-subtree cleanup barrier.
func (f *Fiber) Disposed() <-chan struct{} { return f.disposedDone }

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

// Store returns the services this fiber resolved while active.
func (f *Fiber) Store() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	services := make(map[string]any, len(f.resolvedServices))
	for name, binding := range f.resolvedServices {
		services[name] = binding.getService()
	}
	return services
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
		return nil, newError(ErrInactiveEffect,
			"cannot create effect on inactive context %q", f.Name())
	}
	entry := &effectEntry{meta: &EffectMeta{Label: label}}
	parentEffect := f.current
	var remove func() bool
	if parentEffect != nil {
		// A nested effect is owned by the enclosing one, exactly like Cordis's
		// effect collector: disposing the outer effect disposes its children.
		parentEffect.addChild(entry)
	} else {
		_, remove = f.effects.add(entry)
	}
	f.current = entry
	f.mu.Unlock()

	dispose, panicked := runEffectBody(body)

	f.mu.Lock()
	f.current = parentEffect
	f.mu.Unlock()

	if panicked != nil {
		if parentEffect != nil {
			parentEffect.detach(entry)
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
		if parentEffect != nil {
			parentEffect.detach(entry)
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
	for i := range slices.Backward(children) {
		f.runDisposer(children[i])
	}
}

func (f *Fiber) callDisposer(entry *effectEntry, dispose Disposer) {
	defer func() {
		if reason := recover(); reason != nil {
			f.shared().log.errorf("cordis: panic while disposing %s of plugin %s: %v",
				entry.meta.Label, f.Name(), reason)
		}
	}()
	dispose()
}

// fiberAction selects one transition in the fiber lifecycle state machine.
type fiberAction uint8

const (
	// actionNoop leaves the fiber unchanged: no terminal request, no forced
	// reload, and the resolved dependency epoch matches the recorded one.
	actionNoop fiberAction = iota

	// actionUnload drops an unavailable generation. The resolved epoch is
	// inactive, so the fiber returns to pending after its effects unwind.
	actionUnload

	// actionLoad starts a new generation on a clean fiber. The resolved epoch
	// is satisfiable and differs from the recorded one.
	actionLoad

	// actionCycle replaces a live generation. The fiber is not clean, so the
	// previous effects unwind before the new generation loads.
	actionCycle

	// actionReload forces an unload/reload cycle for Restart and Update,
	// regardless of whether the dependency epoch changed.
	actionReload

	// actionDispose drives a terminal disposal: unload effects and finalize
	// the fiber as disposed.
	actionDispose
)

// sync reconciles one fiber with its pending requests and current dependencies.
func (f *Fiber) sync() {
	action, bindings, epoch := f.plan()
	switch action {
	case actionNoop:
		return
	case actionUnload:
		f.applyUnload()
	case actionDispose:
		f.unload()
	case actionLoad:
		f.applyLoad(bindings, epoch)
	case actionCycle:
		f.applyCycle(bindings, epoch)
	case actionReload:
		f.reload()
	}
}

// plan consumes pending requests and selects the next transition without
// mutating lifecycle state beyond clearing forceReload. bindings and epoch are
// only meaningful for actions that run a load generation.
func (f *Fiber) plan() (action fiberAction, bindings map[string]*serviceBinding, epoch string) {
	if f.root || f.runtime == nil {
		return actionNoop, nil, ""
	}

	disposed, forceReload := f.takeRequest()
	if disposed {
		return actionDispose, nil, ""
	}
	if forceReload {
		return actionReload, nil, ""
	}

	bindings, epoch = f.resolveInjections()

	f.mu.Lock()
	same := epoch == f.epoch
	hasEffects := f.hasEffects
	f.mu.Unlock()

	if same {
		return actionNoop, nil, ""
	}
	if epoch == epochInactive {
		return actionUnload, nil, ""
	}
	if hasEffects {
		return actionCycle, bindings, epoch
	}
	return actionLoad, bindings, epoch
}

// applyUnload records the inactive epoch and unloads the current generation.
func (f *Fiber) applyUnload() {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		f.unload()
		return
	}
	f.epoch = epochInactive
	f.mu.Unlock()
	f.unload()
}

// applyLoad commits the planned epoch and runs the plugin body on a clean
// fiber.
func (f *Fiber) applyLoad(bindings map[string]*serviceBinding, epoch string) {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		f.unload()
		return
	}
	f.epoch = epoch
	f.mu.Unlock()
	f.load(bindings)
}

// applyCycle unloads the previous generation, commits the planned epoch, and
// loads the new generation.
func (f *Fiber) applyCycle(bindings map[string]*serviceBinding, epoch string) {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		f.unload()
		return
	}
	f.epoch = epoch
	f.mu.Unlock()

	f.unload()

	f.mu.Lock()
	disposed := f.disposed
	f.mu.Unlock()
	if disposed {
		f.unload()
		return
	}
	f.load(bindings)
}

// reload forces an unload/reload cycle for Restart and Update. It resets the
// recorded epoch so the next load runs even when dependencies are unchanged.
func (f *Fiber) reload() {
	f.unload()

	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		f.unload()
		return
	}
	f.epoch = epochInactive
	f.err = nil
	f.mu.Unlock()

	f.reconcile()
}

// reconcile resolves the current dependency epoch and drives the load/unload
// tail shared by sync and reload.
func (f *Fiber) reconcile() {
	bindings, epoch := f.resolveInjections()

	f.mu.Lock()
	if f.disposed || epoch == f.epoch {
		f.mu.Unlock()
		return
	}
	f.epoch = epoch
	hasEffects := f.hasEffects
	f.mu.Unlock()

	if epoch == epochInactive {
		f.unload()
		return
	}
	if hasEffects {
		f.unload()

		f.mu.Lock()
		disposed := f.disposed
		f.mu.Unlock()
		if disposed {
			f.unload()
			return
		}
	}
	f.load(bindings)
}

// resolveInjections resolves every injected service and encodes the provider
// identity into one epoch. The snapshot and epoch come from the same pass, so a
// load never runs against an epoch that describes another generation.
func (f *Fiber) resolveInjections() (map[string]*serviceBinding, string) {
	names := f.Inject()
	bindings := make(map[string]*serviceBinding, len(names))
	var builder strings.Builder
	for _, name := range names {
		binding := f.shared().lookupService(f.Ctx.isolateLabel(name))
		if binding == nil {
			return nil, epochInactive
		}
		bindings[name] = binding
		builder.WriteByte(':')
		builder.WriteString(strconv.Itoa(binding.provider.UID))
	}
	return bindings, builder.String()
}

// load runs one plugin generation from a dependency snapshot resolved by plan.
//
// The caller is the refresh owner. load still re-checks disposed and provider
// liveness because callbacks and concurrent disposal can invalidate the plan.
func (f *Fiber) load(bindings map[string]*serviceBinding) {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		return
	}
	f.hasEffects = true
	f.mu.Unlock()
	if !f.setState(StateLoading) {
		return
	}
	for _, binding := range bindings {
		if binding.live() {
			continue
		}
		// A provider vanished while the load was being prepared. Ask the
		// refresh owner to re-evaluate before running the plugin body.
		f.refresh()
		return
	}

	f.mu.Lock()
	f.resolvedServices = bindings
	raw := f.rawConfig
	f.mu.Unlock()

	config, err := f.runtime.definition.ResolveConfig(raw)
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
	if f.disposed {
		f.mu.Unlock()
		f.unload()
		return
	}
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
	return f.runtime.definition.Run(f.Ctx, config)
}

func (f *Fiber) fail(err error) {
	// A failed load must not leak the effects it managed to register before
	// failing, nor keep a service name it already provided occupied. Cordis does
	// the same: _reload sets epoch=INACTIVE on error, which drives _unload.
	f.unload()

	f.mu.Lock()
	f.err = err
	disposed := f.disposed
	f.mu.Unlock()
	if disposed {
		return
	}
	f.setState(StateFailed)
	f.shared().log.errorf("cordis: plugin %s failed to load: %v", f.Name(), err)
}

// unload releases the current generation of effects.
//
// The caller is the refresh owner. unload is idempotent: a disposed fiber
// finishes through finalizeDispose, otherwise it returns to pending.
func (f *Fiber) unload() {
	f.mu.Lock()
	if f.hasEffects {
		f.hasEffects = false
		f.mu.Unlock()
	} else {
		disposed := f.disposed
		f.mu.Unlock()
		if disposed {
			f.finalizeDispose()
		}
		return
	}

	f.setState(StateUnloading)
	for _, entry := range f.effects.clear() {
		f.runDisposer(entry)
	}
	f.mu.Lock()
	f.resolvedServices = nil
	f.config = nil
	disposed := f.disposed
	f.mu.Unlock()
	if disposed {
		f.finalizeDispose()
		return
	}
	f.setState(StatePending)
}

// finalizeDispose drives a disposed fiber to its terminal state and releases
// the slot it occupied in the parent's effect list. It is idempotent.
func (f *Fiber) finalizeDispose() {
	f.disposeOnce.Do(func() {
		f.setState(StateDisposed)

		f.mu.Lock()
		parentEffectDisposer := f.parentEffectDisposer
		f.parentEffectDisposer = nil
		f.mu.Unlock()
		if parentEffectDisposer != nil {
			parentEffectDisposer()
		}
		close(f.disposedDone)
	})
}

func (f *Fiber) setState(state FiberState) bool {
	f.mu.Lock()
	if f.disposed && state != StateUnloading && state != StateDisposed {
		f.mu.Unlock()
		return false
	}
	old := f.state
	f.state = state
	f.mu.Unlock()
	if old == state {
		return true
	}
	f.shared().bus.emitInternal("internal/status", &StatusEvent{Fiber: f, Old: old})

	// Only a change of service availability wakes dependents.
	if (old == StateActive) != (state == StateActive) {
		f.notifyProvided()
	}
	return true
}

// notifyProvided re-evaluates every fiber that injects a service this fiber
// owns, which is what makes dependency replacement propagate.
func (f *Fiber) notifyProvided() {
	for _, name := range f.shared().providedNames(f) {
		f.shared().notify(name, f.Ctx.isolateLabel(name))
	}
}

// Dispose starts unloading the plugin and releases everything it registered.
// It is idempotent. Cancellation happens immediately; when a refresh transition
// is already running, the effect teardown is deferred to that loop. Use Disposed
// to wait for the deferred teardown.
func (f *Fiber) Dispose() {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		return
	}
	f.disposed = true
	// Mark a rerun before the cancel/remove/emit steps so a running owner can
	// start teardown without waiting for them. refresh clears the marker on the
	// first pass when no owner is running.
	f.dirty = true
	f.mu.Unlock()

	// Stop dependent goroutines immediately, then unwind effects.
	f.cancel()
	close(f.done)

	if f.runtime != nil {
		f.shared().removeFiber(f.runtime, f)
		f.shared().bus.emitInternal("internal/plugin", &PluginEvent{Fiber: f})
	}

	if f.root {
		f.unload()
		return
	}
	f.refresh()
}

// Restart requests an unload/reload cycle with the current config. When a
// refresh transition is already running, the request is queued and Restart
// returns without waiting for the reload.
func (f *Fiber) Restart() error {
	if err := f.assertActive(); err != nil {
		return err
	}
	f.mu.Lock()
	f.forceReload = true
	f.mu.Unlock()
	f.refresh()
	return f.Error()
}

// Update replaces the raw config and requests a restart. When a refresh
// transition is already running, the request is queued and Update returns
// without waiting for the reload.
func (f *Fiber) Update(config any) error {
	if err := f.assertActive(); err != nil {
		return err
	}
	f.mu.Lock()
	f.rawConfig = config
	f.err = nil
	f.forceReload = true
	f.mu.Unlock()
	f.refresh()
	return f.Error()
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

func (c *core) addFiber(runtime *runtime, fiber *Fiber) {
	c.mu.Lock()
	defer c.mu.Unlock()
	runtime.fibers = append(runtime.fibers, fiber)
}

func (c *core) removeFiber(runtime *runtime, fiber *Fiber) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, candidate := range runtime.fibers {
		if candidate == fiber {
			runtime.fibers = append(runtime.fibers[:i], runtime.fibers[i+1:]...)
			break
		}
	}
	if len(runtime.fibers) == 0 {
		delete(c.runtimes, runtime.definition)
	}
}

// snapshotFibers returns a copy of every live fiber.
func (c *core) snapshotFibers() []*Fiber {
	c.mu.Lock()
	defer c.mu.Unlock()
	var fibers []*Fiber
	for _, runtime := range c.runtimes {
		fibers = append(fibers, runtime.fibers...)
	}
	return fibers
}
