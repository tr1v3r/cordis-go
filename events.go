package cordis

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// eventListener is one registered handler.
type eventListener struct {
	name    string
	ctx     *Context
	fn      func(payload any, next func(any) any) any
	prepend bool
	global  bool
	once    bool

	// fired claims the listener for a single dispatch. Only the dispatcher that
	// flips it from false to true runs the body, so two dispatchers holding a
	// snapshot taken before the listener left the bus cannot both run it.
	fired atomic.Bool
	// released records that a once-listener was retired. It is published before
	// cleanup is installed, so a dispatch that wins that race can still release
	// the effect entry and the registration completes the release afterwards.
	released atomic.Bool
	// cleanup removes the listener's effect entry once a once-listener has
	// fired, so Effects() stops reporting a handler that can never run again.
	cleanup atomic.Pointer[Disposer]
}

// eventBus is the per-application event dispatcher.
type eventBus struct {
	shared *core
	mu     sync.RWMutex
	hooks  map[string][]*eventListener
}

func newEventBus(shared *core) *eventBus {
	return &eventBus{shared: shared, hooks: map[string][]*eventListener{}}
}

type eventOptions struct {
	prepend bool
	global  bool
	once    bool
}

// EventOption customizes listener registration.
type EventOption func(*eventOptions)

// Prepend inserts the listener before existing listeners of the same event.
func Prepend() EventOption {
	return func(o *eventOptions) { o.prepend = true }
}

// Global marks the listener as scope-agnostic: it receives events dispatched
// through the Scoped variants even from another isolation scope.
func Global() EventOption {
	return func(o *eventOptions) { o.global = true }
}

// WithOnce removes the listener after its first invocation and releases the
// effect entry that keeps it alive, so Effects() stops reporting it.
func WithOnce() EventOption {
	return func(o *eventOptions) { o.once = true }
}

// On registers a typed listener owned by the context's explicit effect scope,
// or by its fiber when no such scope is present.
func (c *Context) On[E any](name string, fn func(E), opts ...EventOption) Disposer {
	return c.on(name, func(payload any, _ func(any) any) any {
		fn(assertPayload[E](name, payload))
		return nil
	}, opts...)
}

// OnOnce registers a typed listener that runs at most once.
func (c *Context) OnOnce[E any](name string, fn func(E), opts ...EventOption) Disposer {
	// Copy: appending to the caller's slice in place could overwrite a
	// subsequent option they intend to reuse.
	options := append(append([]EventOption(nil), opts...), WithOnce())
	return c.On(name, fn, options...)
}

// OnValue registers a typed listener whose return value participates in Bail
// and Serial. A non-nil, non-false return value bails the dispatch.
func (c *Context) OnValue[E any](name string, fn func(E) any, opts ...EventOption) Disposer {
	return c.on(name, func(payload any, _ func(any) any) any {
		return fn(assertPayload[E](name, payload))
	}, opts...)
}

// OnWaterfall registers a listener that wraps the rest of a Waterfall chain.
// Calling next continues the chain; not calling it vetoes the remainder.
//
// A chain settles at most once. Calling next again after the chain already
// reached final returns the settled result instead of running final twice, and
// a listener that panics after next returned is reported like any other failing
// listener without disturbing that result.
func (c *Context) OnWaterfall[E any](name string, fn func(E, func(E) any) any,
	opts ...EventOption) Disposer {
	return c.on(name, func(payload any, next func(any) any) any {
		return fn(assertPayload[E](name, payload), func(value E) any { return next(value) })
	}, opts...)
}

func assertPayload[E any](name string, payload any) E {
	value, ok := payload.(E)
	if !ok {
		var zero E
		panic(fmt.Sprintf("event %q: payload has type %T, want %T", name, payload, zero))
	}
	return value
}

func (c *Context) on(name string, fn func(any, func(any) any) any, opts ...EventOption) Disposer {
	options := &eventOptions{}
	for _, opt := range opts {
		opt(options)
	}
	listener := &eventListener{
		name:    name,
		ctx:     c,
		fn:      fn,
		prepend: options.prepend,
		global:  options.global,
		once:    options.once,
	}
	bus := c.shared.bus
	disposer := c.effect(fmt.Sprintf("ctx.On(%q)", name), func(*effectEntry) Disposer {
		bus.add(listener)
		return func() { bus.remove(listener) }
	})
	cleanup := disposer
	listener.cleanup.Store(&cleanup)
	if options.once && listener.released.Load() {
		// A dispatch retired the listener while its cleanup was still
		// uninstalled and could not run it; the effect entry would otherwise
		// outlive a handler that can never run again.
		cleanup()
	}
	return disposer
}

func (b *eventBus) add(listener *eventListener) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if listener.prepend {
		b.hooks[listener.name] = append([]*eventListener{listener}, b.hooks[listener.name]...)
		return
	}
	b.hooks[listener.name] = append(b.hooks[listener.name], listener)
}

func (b *eventBus) remove(listener *eventListener) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hooks := b.hooks[listener.name]
	for i, candidate := range hooks {
		if candidate == listener {
			b.hooks[listener.name] = append(hooks[:i], hooks[i+1:]...)
			break
		}
	}
	if len(b.hooks[listener.name]) == 0 {
		delete(b.hooks, listener.name)
	}
}

func (b *eventBus) snapshot(name string) []*eventListener {
	b.mu.RLock()
	defer b.mu.RUnlock()
	source := b.hooks[name]
	out := make([]*eventListener, len(source))
	copy(out, source)
	return out
}

func (b *eventBus) selectListeners(name string, filter func(*eventListener) bool) []*eventListener {
	listeners := b.snapshot(name)
	if filter == nil {
		return listeners
	}
	out := make([]*eventListener, 0, len(listeners))
	for _, listener := range listeners {
		if listener.global || filter(listener) {
			out = append(out, listener)
		}
	}
	return out
}

// claim reserves a listener for one dispatch and reports whether the caller may
// run it. A listener that may run more than once, or one that no dispatcher
// claimed yet, is always handed to the caller.
func claim(listener *eventListener) bool {
	return !listener.once || listener.fired.CompareAndSwap(false, true)
}

// releaseOnce retires a once-listener: it leaves the bus and its effect entry is
// disposed, so Effects() stops reporting a handler that can never run again.
//
// Every dispatch path releases through this method, so no path can drop the
// effect entry. It tolerates being called before the registration installed the
// cleanup: the release is published either way and the registration disposes the
// entry itself. Disposers produced by this package are idempotent, so both sides
// may run it.
func (b *eventBus) releaseOnce(listener *eventListener) {
	if !listener.released.CompareAndSwap(false, true) {
		return
	}
	b.remove(listener)
	if cleanup := listener.cleanup.Load(); cleanup != nil {
		(*cleanup)()
	}
}

func (b *eventBus) invoke(listener *eventListener, payload any) (result any, err error) {
	if !claim(listener) {
		return nil, nil
	}
	once := listener.once
	defer func() {
		if once {
			b.releaseOnce(listener)
		}
		if reason := recover(); reason != nil {
			err = fmt.Errorf("event %q listener panicked: %v", listener.name, reason)
			b.shared.log.errorf("%v", err)
		}
	}()
	return listener.fn(payload, nil), nil
}

func (b *eventBus) emitInternal(name string, payload any) {
	for _, listener := range b.snapshot(name) {
		// invoke reports a listener panic itself; logging the returned error
		// here too would report the same panic twice.
		_, _ = b.invoke(listener, payload)
	}
}

// scopeFilter builds the isolation filter used by the Scoped dispatch
// variants: only listeners whose scope for scopeName matches the emitter's
// receive the event, unless they were registered Global.
func (c *Context) scopeFilter(scopeName string) func(*eventListener) bool {
	want := c.isolateLabel(scopeName)
	return func(listener *eventListener) bool {
		return listener.ctx.isolateLabel(scopeName) == want
	}
}

// Emit dispatches an event synchronously and ignores listener return values.
func (c *Context) Emit[E any](name string, payload E) {
	emitWith(c, name, payload, nil)
}

// EmitScoped dispatches an event only to listeners in the same isolation scope,
// mirroring Cordis's service-scoped filtering.
func (c *Context) EmitScoped[E any](scopeName, name string, payload E) {
	emitWith(c, name, payload, c.scopeFilter(scopeName))
}

func emitWith[E any](c *Context, name string, payload E, filter func(*eventListener) bool) {
	for _, listener := range c.shared.bus.selectListeners(name, filter) {
		// invoke reports a listener panic itself; logging the returned error
		// here too would report the same panic twice.
		_, _ = c.shared.bus.invoke(listener, payload)
	}
}

// Bail dispatches an event and stops at the first listener returning a non-nil
// value, which is returned with bailed=true.
func (c *Context) Bail[E any](name string, payload E) (result any, bailed bool) {
	return bailWith(c, name, payload, nil)
}

// BailScoped is Bail restricted to the same isolation scope.
func (c *Context) BailScoped[E any](scopeName, name string, payload E) (any, bool) {
	return bailWith(c, name, payload, c.scopeFilter(scopeName))
}

func bailWith[E any](c *Context, name string, payload E,
	filter func(*eventListener) bool) (result any, bailed bool) {
	for _, listener := range c.shared.bus.selectListeners(name, filter) {
		value, err := c.shared.bus.invoke(listener, payload)
		if err != nil {
			// invoke already reported the panic; keep dispatching.
			continue
		}
		if value != nil && value != false {
			return value, true
		}
	}
	return nil, false
}

// Serial is Bail for synchronous listeners; it exists so ported code keeps the
// Cordis spelling. Awaiting async listeners is unnecessary in Go.
func (c *Context) Serial[E any](name string, payload E) (any, bool) {
	return c.Bail(name, payload)
}

// SerialScoped is Serial restricted to the same isolation scope.
func (c *Context) SerialScoped[E any](scopeName, name string, payload E) (any, bool) {
	return c.BailScoped(scopeName, name, payload)
}

// Parallel dispatches an event to every listener concurrently and joins the
// panics of failing listeners into one error.
func (c *Context) Parallel[E any](name string, payload E) error {
	return parallelWith(c, name, payload, nil)
}

// ParallelScoped is Parallel restricted to the same isolation scope.
func (c *Context) ParallelScoped[E any](scopeName, name string, payload E) error {
	return parallelWith(c, name, payload, c.scopeFilter(scopeName))
}

func parallelWith[E any](c *Context, name string, payload E,
	filter func(*eventListener) bool) error {
	listeners := c.shared.bus.selectListeners(name, filter)
	var wg sync.WaitGroup
	errs := make([]error, len(listeners))
	for i, listener := range listeners {
		wg.Add(1)
		go func(index int, listener *eventListener) {
			defer wg.Done()
			if _, err := c.shared.bus.invoke(listener, payload); err != nil {
				errs[index] = err
			}
		}(i, listener)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Waterfall composes listeners around final: each listener may call next to
// continue the chain, and the outermost listener's return value wins.
func (c *Context) Waterfall[E any](name string, payload E, final func(E) any) any {
	return waterfallWith(c, name, payload, final, nil)
}

// WaterfallScoped is Waterfall restricted to the same isolation scope.
func (c *Context) WaterfallScoped[E any](scopeName, name string, payload E, final func(E) any) any {
	return waterfallWith(c, name, payload, final, c.scopeFilter(scopeName))
}

func waterfallWith[E any](c *Context, name string, payload E, final func(E) any,
	filter func(*eventListener) bool) any {
	listeners := c.shared.bus.selectListeners(name, filter)
	tail := &waterfallStep{name: name, bus: c.shared.bus}
	if final != nil {
		tail.final = func(value any) any { return final(assertPayload[E](name, value)) }
	}
	for i := len(listeners) - 1; i >= 0; i-- {
		tail = &waterfallStep{
			name:     name,
			bus:      c.shared.bus,
			listener: listeners[i],
			next:     tail,
		}
	}
	return tail.call(payload)
}

// waterfallStep owns one continuation in a waterfall chain. Each listener gets
// the call method of its successor as next, so synchronous recursive next calls
// enter a different step and never wait on themselves. Calls to the same next
// while that step is running wait for its first caller: this makes concurrent or
// saved next calls deterministic, and no listener or final can run twice.
type waterfallStep struct {
	name     string
	bus      *eventBus
	listener *eventListener
	final    func(any) any
	next     *waterfallStep

	mu       sync.Mutex
	started  bool
	finished bool
	done     chan struct{}
	result   any
}

func (s *waterfallStep) call(value any) any {
	s.mu.Lock()
	if s.finished {
		result := s.result
		s.mu.Unlock()
		return result
	}
	if s.started {
		done := s.done
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		result := s.result
		s.mu.Unlock()
		return result
	}
	s.started = true
	s.done = make(chan struct{})
	s.mu.Unlock()

	result := s.run(value)

	s.mu.Lock()
	s.result = result
	s.finished = true
	close(s.done)
	s.mu.Unlock()
	return result
}

func (s *waterfallStep) run(value any) any {
	if s.listener == nil {
		return s.runFinal(value)
	}
	result, invoked, err := s.invokeListener(value)
	if !invoked {
		return s.next.call(value)
	}
	if err != nil {
		s.bus.shared.log.errorf("%v", err)
		return s.next.call(value)
	}
	return result
}

func (s *waterfallStep) invokeListener(value any) (result any, invoked bool, err error) {
	if !claim(s.listener) {
		return nil, false, nil
	}
	invoked = true
	defer func() {
		if s.listener.once {
			s.bus.releaseOnce(s.listener)
		}
		if reason := recover(); reason != nil {
			err = fmt.Errorf("event %q listener panicked: %v", s.name, reason)
		}
	}()
	result = s.listener.fn(value, s.next.call)
	return result, invoked, nil
}

func (s *waterfallStep) runFinal(value any) (result any) {
	if s.final == nil {
		return nil
	}
	defer func() {
		if reason := recover(); reason != nil {
			result = nil
			s.bus.shared.log.errorf("event %q final panicked: %v", s.name, reason)
		}
	}()
	return s.final(value)
}
