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

	// runtime, inject and root are fixed when the fiber is constructed.
	runtime *runtime
	inject  map[string]struct{}
	root    bool

	// Lifetime plumbing, also fixed at construction. Dispose calls cancel and
	// closes done; finalizeDispose runs the terminal transition once.
	lifecycleCtx context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	disposeOnce  sync.Once

	// disposables is the fiber's owned effect tree: each entry pairs a label
	// with the disposer that releases it, and nested effects become children.
	// The pointer is fixed at construction and the list synchronizes itself,
	// so mu does not guard it. The public Effects() exposes only the metadata,
	// not the disposers.
	disposables *disposableList

	// mu guards the mutable fields below.
	mu sync.Mutex

	// Current generation: lifecycle state, dependency epoch, and what load
	// derived from that epoch.
	state            FiberState
	err              error
	epoch            string
	config           any
	rawConfig        any
	resolvedServices map[string]*serviceBinding

	// Transition driver: busy marks the owner of a pass, dirty records a rerun
	// request, and forceReload/disposed are requests consumed by sync.
	busy        bool
	dirty       bool
	forceReload bool
	disposed    bool

	// live marks a generation claimed by load and not yet released by unload.
	// It is set before the body runs and cleared before disposers run, so it
	// stays true for an active fiber even when it registered no effects.
	live bool

	// current is the effect whose body is running; registrations inside it
	// become its children. It is always nil or an entry whose body is still
	// running, so a registration can never nest under an effect that is already
	// finished or unwound.
	current *effectEntry

	// parentEffectDisposer releases this fiber's slot in the parent's tree.
	parentEffectDisposer Disposer
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
		disposables:      newDisposableList(),
		live:             true,
		done:             make(chan struct{}),
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
		disposables:  newDisposableList(),
		done:         make(chan struct{}),
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
		// The parent unloaded while this fiber was attaching, so Dispose
		// already released everything the fiber owns. It never takes a slot in
		// its runtime: a fiber attached after its own disposal could not be
		// removed again.
		f.mu.Unlock()
		parentEffectDisposer()
		f.shared().discardRuntime(f.runtime)
		return nil
	}
	f.parentEffectDisposer = parentEffectDisposer
	f.mu.Unlock()
	f.shared().attachFiber(f.runtime, f)
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
	entries := f.disposables.snapshot()
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
//
// Nesting is decided per registration and never on stale state: the entry
// adopts a nested effect only while its own body runs, a registration that
// finds a finished enclosing effect becomes a fiber-level one instead, and the
// scope is restored to the nearest body that is still running (or to the fiber
// level). Concurrent registrations therefore cannot attach an effect to an
// entry that will never unwind it.
func (f *Fiber) tryEffect(label string, body func() Disposer) (Disposer, error) {
	f.mu.Lock()
	if f.disposed || f.state == StateUnloading {
		f.mu.Unlock()
		return nil, newError(ErrInactiveEffect,
			"cannot create effect on inactive context %q", f.Name())
	}
	entry := &effectEntry{meta: &EffectMeta{Label: label}, running: true}
	parentEffect := f.current
	var remove func() bool
	if parentEffect != nil && !parentEffect.adopt(entry) {
		// The enclosing effect finished (or was unwound) meanwhile; owning the
		// entry there would leave its disposer unreachable.
		parentEffect = nil
	}
	if parentEffect == nil {
		_, remove = f.disposables.add(entry)
	}
	f.current = entry
	f.mu.Unlock()

	dispose, panicked := runEffectBody(body)
	entry.finishBody()

	f.mu.Lock()
	if f.current == entry {
		// Restore the enclosing scope. A registration from another goroutine may
		// be current by now: leave it alone rather than clobbering it with this
		// call's captured parent.
		f.current = parentEffect.runningAncestor()
	}
	f.mu.Unlock()

	if panicked != nil {
		if parentEffect != nil {
			parentEffect.detach(entry)
		} else {
			remove()
		}
		// The body panicked before producing a disposer, but the effects it
		// registered before that still have to unwind.
		f.unwindChildren(entry.abandon())
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
	f.unwindChildren(children)
}

// unwindChildren disposes nested effects newest-first, after their owner.
func (f *Fiber) unwindChildren(children []*effectEntry) {
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

// The per-fiber transition driver gives one caller ownership of a transition
// pass while other callers record that another pass is needed instead of
// starting a second one.

// refresh requests one transition pass. If another pass is already running,
// the request is recorded for that owner instead of starting a second one.
func (f *Fiber) refresh() {
	if f.beginTransition() {
		f.drive()
	}
}

// drive drains transition passes until no caller asked for another one.
func (f *Fiber) drive() {
	for {
		// Begin a pass: clear the rerun marker for this transition.
		f.mu.Lock()
		f.dirty = false
		f.mu.Unlock()

		f.sync()

		// End a pass: keep ownership when another request arrived while sync
		// was running; otherwise release it and stop.
		f.mu.Lock()
		dirty := f.dirty
		if dirty {
			f.mu.Unlock()
			continue
		}
		f.busy = false
		f.mu.Unlock()
		return
	}
}

// beginTransition tries to become the transition owner. It reports false when
// another pass is running; that owner is marked for one more pass.
func (f *Fiber) beginTransition() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy {
		f.dirty = true
		return false
	}
	f.busy = true
	return true
}

// sync reconciles one fiber with its pending requests and current dependencies.
func (f *Fiber) sync() {
	// Root and runtime-less fibers do not own a plugin generation.
	if f.root || f.runtime == nil {
		return
	}

	// Consume pending requests first. Disposal is terminal and forceReload
	// must be observed even when the dependency epoch is unchanged.
	f.mu.Lock()
	disposed := f.disposed
	forceReload := f.forceReload
	f.forceReload = false
	f.mu.Unlock()

	// Dispose wins over every other transition; unload finalizes the fiber.
	if disposed {
		f.unload()
		return
	}
	// Restart and Update force a new generation even when dependencies match.
	if forceReload {
		f.reload()
		return
	}

	// Resolve the current dependency generation. The returned epoch is both
	// the decision key and the identity of this generation.
	bindings, epoch := f.resolveInjections()

	// Snapshot the comparison inputs under one lock so the decision below is
	// based on a single observed state.
	f.mu.Lock()
	same := epoch == f.epoch
	live := f.live
	f.mu.Unlock()

	switch {
	case same:
		// The recorded generation still matches the dependencies in place.
		return
	case epoch == epochInactive:
		// Dependencies are gone; release the current generation.
		f.applyUnload()
	case live:
		// A new generation is available, but the old one is still live.
		// reload unloads it first and resolves the new world afterwards.
		f.reload()
	default:
		// The fiber is clean; start the resolved generation directly.
		f.applyLoad(bindings, epoch)
	}
}

// applyUnload records the inactive epoch and unloads the current generation.
func (f *Fiber) applyUnload() {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		return
	}
	f.epoch = epochInactive
	f.mu.Unlock()
	f.unload()
}

// applyLoad runs the plugin body for a resolved generation on a clean fiber.
func (f *Fiber) applyLoad(bindings map[string]*serviceBinding, epoch string) {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()
	f.load(bindings, epoch)
}

// reload forces an unload/reload cycle for Restart and Update. It forgets the
// recorded generation so the new one loads even when dependencies are unchanged.
func (f *Fiber) reload() {
	f.unload()

	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		// Dispose set dirty; the refresh owner will run the dispose action next.
		return
	}
	f.epoch = epochInactive
	f.err = nil
	f.mu.Unlock()

	// Resolve after the old generation is gone: unload may have changed the
	// service registry, so pre-unload bindings can already be stale.
	bindings, epoch := f.resolveInjections()
	if epoch == epochInactive {
		return
	}

	// reload unloaded the previous generation at entry, so the fiber is clean
	// here. load re-checks disposed before it starts the new body.
	f.load(bindings, epoch)
}

// resolveInjections resolves every injected service and encodes the provider
// identity and the binding identity into one epoch. The snapshot and epoch come
// from the same pass, so a load never runs against an epoch that describes
// another generation.
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
		// The binding sequence matters as much as the provider: a provider that
		// releases its registration and provides the name again keeps its UID,
		// but a dependent pinned to the released object must still reload.
		builder.WriteByte(':')
		builder.WriteString(strconv.Itoa(binding.provider.UID))
		builder.WriteByte('.')
		builder.WriteString(strconv.Itoa(binding.seq))
	}
	return bindings, builder.String()
}

// load runs one plugin generation from a dependency snapshot resolved by sync.
//
// The caller is the refresh owner. load still re-checks disposed and provider
// liveness because callbacks and concurrent disposal can invalidate the decision.
func (f *Fiber) load(bindings map[string]*serviceBinding, epoch string) {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		return
	}
	f.live = true
	f.mu.Unlock()
	if !f.setState(StateLoading) {
		return
	}
	// Don't run the body against a provider disposed after resolution. Defense,
	// not correctness: later deaths go through unload's unregister+notify,
	// which marks a rerun; the check stays because it is cheap.
	for _, binding := range bindings {
		if binding.live() {
			continue
		}
		// Nothing of this generation started, so give the claim back: record the
		// inactive epoch and return to pending. Without that the rerun pass would
		// compare the re-resolved epoch against a committed one, match, and leave
		// the fiber loading forever with no body and no effects.
		f.mu.Lock()
		f.live = false
		f.epoch = epochInactive
		f.mu.Unlock()
		f.setState(StatePending)
		f.refresh()
		return
	}

	// The epoch is committed only now, together with the snapshot it describes:
	// a generation that never ran its body must not look like the loaded one.
	f.mu.Lock()
	f.epoch = epoch
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
	if f.live {
		f.live = false
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
	for _, entry := range f.disposables.clear() {
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

// attachFiber publishes a fiber under its runtime and gives back the claim its
// load took on it.
func (c *core) attachFiber(runtime *runtime, fiber *Fiber) {
	c.mu.Lock()
	defer c.mu.Unlock()
	runtime.fibers = append(runtime.fibers, fiber)
	runtime.claims--
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
	if runtime.claims == 0 && len(runtime.fibers) == 0 {
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
