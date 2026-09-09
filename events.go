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

// WithOnce removes the listener after its first invocation.
func WithOnce() EventOption {
	return func(o *eventOptions) { o.once = true }
}

// On registers a typed listener owned by the context's fiber.
func On[E any](c *Context, name string, fn func(E), opts ...EventOption) Disposer {
	return c.on(name, func(payload any, _ func(any) any) any {
		fn(assertPayload[E](name, payload))
		return nil
	}, opts...)
}

// OnOnce registers a typed listener that runs at most once.
func OnOnce[E any](c *Context, name string, fn func(E), opts ...EventOption) Disposer {
	// Copy: appending to the caller's slice in place could overwrite a
	// subsequent option they intend to reuse.
	options := append(append([]EventOption(nil), opts...), WithOnce())
	return On(c, name, fn, options...)
}

// OnValue registers a typed listener whose return value participates in Bail
// and Serial. A non-nil, non-false return value bails the dispatch.
func OnValue[E any](c *Context, name string, fn func(E) any, opts ...EventOption) Disposer {
	return c.on(name, func(payload any, _ func(any) any) any {
		return fn(assertPayload[E](name, payload))
	}, opts...)
}

// OnWaterfall registers a listener that wraps the rest of a Waterfall chain.
// Calling next continues the chain; not calling it vetoes the remainder.
func OnWaterfall[E any](c *Context, name string, fn func(E, func(E) any) any, opts ...EventOption) Disposer {
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

// On registers an untyped listener owned by the context's fiber.
func (c *Context) On(name string, fn func(payload any), opts ...EventOption) Disposer {
	return c.on(name, func(payload any, _ func(any) any) any {
		fn(payload)
		return nil
	}, opts...)
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
	disposer := c.fiber.effect(fmt.Sprintf("ctx.On(%q)", name), func() Disposer {
		bus.add(listener)
		return func() { bus.remove(listener) }
	})
	cleanup := disposer
	listener.cleanup.Store(&cleanup)
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

func (b *eventBus) invoke(listener *eventListener, payload any) (result any, err error) {
	once := listener.once
	if once {
		b.remove(listener)
	}
	defer func() {
		if once {
			if cleanup := listener.cleanup.Load(); cleanup != nil {
				(*cleanup)()
			}
		}
		if reason := recover(); reason != nil {
			err = fmt.Errorf("event %q listener panicked: %v", listener.name, reason)
			b.shared.log.errorf("cordis: %v", err)
		}
	}()
	return listener.fn(payload, nil), nil
}

func (b *eventBus) emitInternal(name string, payload any) {
	for _, listener := range b.snapshot(name) {
		if _, err := b.invoke(listener, payload); err != nil {
			b.shared.log.errorf("cordis: %v", err)
		}
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
func Emit[E any](c *Context, name string, payload E) {
	emitWith(c, name, payload, nil)
}

// EmitScoped dispatches an event only to listeners in the same isolation scope,
// mirroring Cordis's service-scoped filtering.
func EmitScoped[E any](c *Context, scopeName, name string, payload E) {
	emitWith(c, name, payload, c.scopeFilter(scopeName))
}

func emitWith[E any](c *Context, name string, payload E, filter func(*eventListener) bool) {
	for _, listener := range c.shared.bus.selectListeners(name, filter) {
		if _, err := c.shared.bus.invoke(listener, payload); err != nil {
			c.shared.log.errorf("cordis: %v", err)
		}
	}
}

// Bail dispatches an event and stops at the first listener returning a non-nil
// value, which is returned with bailed=true.
func Bail[E any](c *Context, name string, payload E) (result any, bailed bool) {
	return bailWith(c, name, payload, nil)
}

// BailScoped is Bail restricted to the same isolation scope.
func BailScoped[E any](c *Context, scopeName, name string, payload E) (any, bool) {
	return bailWith(c, name, payload, c.scopeFilter(scopeName))
}

func bailWith[E any](c *Context, name string, payload E, filter func(*eventListener) bool) (result any, bailed bool) {
	for _, listener := range c.shared.bus.selectListeners(name, filter) {
		value, err := c.shared.bus.invoke(listener, payload)
		if err != nil {
			c.shared.log.errorf("cordis: %v", err)
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
func Serial[E any](c *Context, name string, payload E) (any, bool) {
	return Bail(c, name, payload)
}

// SerialScoped is Serial restricted to the same isolation scope.
func SerialScoped[E any](c *Context, scopeName, name string, payload E) (any, bool) {
	return BailScoped(c, scopeName, name, payload)
}

// Parallel dispatches an event to every listener concurrently and joins the
// panics of failing listeners into one error.
func Parallel[E any](c *Context, name string, payload E) error {
	return parallelWith(c, name, payload, nil)
}

// ParallelScoped is Parallel restricted to the same isolation scope.
func ParallelScoped[E any](c *Context, scopeName, name string, payload E) error {
	return parallelWith(c, name, payload, c.scopeFilter(scopeName))
}

func parallelWith[E any](c *Context, name string, payload E, filter func(*eventListener) bool) error {
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
func Waterfall[E any](c *Context, name string, payload E, final func(E) any) any {
	return waterfallWith(c, name, payload, final, nil)
}

// WaterfallScoped is Waterfall restricted to the same isolation scope.
func WaterfallScoped[E any](c *Context, scopeName, name string, payload E, final func(E) any) any {
	return waterfallWith(c, name, payload, final, c.scopeFilter(scopeName))
}

func waterfallWith[E any](c *Context, name string, payload E, final func(E) any, filter func(*eventListener) bool) any {
	listeners := c.shared.bus.selectListeners(name, filter)
	index := 0
	var next func(any) any
	next = func(value any) any {
		for index < len(listeners) {
			listener := listeners[index]
			index++
			if listener.once {
				c.shared.bus.remove(listener)
			}
			result, err := func() (result any, err error) {
				defer func() {
					if reason := recover(); reason != nil {
						err = fmt.Errorf("event %q listener panicked: %v", listener.name, reason)
					}
				}()
				return listener.fn(value, next), nil
			}()
			if err != nil {
				c.shared.log.errorf("cordis: %v", err)
				continue
			}
			return result
		}
		if final == nil {
			return nil
		}
		return final(assertPayload[E](name, value))
	}
	return next(payload)
}
