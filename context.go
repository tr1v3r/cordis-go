package cordis

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// Option configures the root context created by New.
type Option func(*core)

// WithWriter sets the destination of the root logger.
func WithWriter(w io.Writer) Option {
	return func(c *core) { c.logWriter = w }
}

// WithLevel sets the root logger level.
func WithLevel(level Level) Option {
	return func(c *core) { c.logLevel = level }
}

// core is the state shared by every context of one application. It is reached
// through Context.Root() so that child contexts stay cheap to create.
type core struct {
	mu              sync.Mutex
	serviceBindings map[string]*serviceBinding
	runtimes        map[definition]*runtime
	counter         int
	scopeSeq        int
	root            *Context
	bus             *eventBus
	log             *loggerService
	logWriter       io.Writer
	logLevel        Level
}

// New creates a root context and installs the built-in services.
//
// The returned context owns the whole application: disposing it disposes every
// plugin fiber loaded beneath it.
func New(opts ...Option) *Context {
	appCore := &core{
		serviceBindings: map[string]*serviceBinding{},
		runtimes:        map[definition]*runtime{},
		logWriter:       io.Discard,
		logLevel:        LevelInfo,
	}
	for _, opt := range opts {
		opt(appCore)
	}

	rootCtx := &Context{shared: appCore, name: "root"}
	rootFiber := newRootFiber(rootCtx)
	rootCtx.fiber = rootFiber
	appCore.root = rootCtx
	appCore.bus = newEventBus(appCore)
	appCore.log = newLoggerService(appCore.logWriter, appCore.logLevel)

	// Built-in services are ordinary services: plugins may inject them by name
	// and they disappear with the root fiber like any other effect.
	if _, err := Provide[Registry](rootCtx, "registry", appCore.registryFacade()); err != nil {
		panic(err)
	}
	if _, err := Provide[*EventService](rootCtx, "events",
		&EventService{ctx: rootCtx}); err != nil {
		panic(err)
	}
	if _, err := Provide[*LoggerService](rootCtx, "logger",
		&LoggerService{svc: appCore.log}); err != nil {
		panic(err)
	}
	return rootCtx
}

// Context is a dependency container and a lifecycle scope.
//
// Every context belongs to a fiber. A plugin receives the fiber's own context;
// disposing that fiber unwinds every effect the plugin registered through it.
type Context struct {
	shared  *core
	parent  *Context
	fiber   *Fiber
	isolate map[string]string
	name    string
}

// Root returns the application root context.
func (c *Context) Root() *Context { return c.shared.root }

// Parent returns the parent context, or nil for the root.
func (c *Context) Parent() *Context { return c.parent }

// Fiber returns the fiber that owns this context.
func (c *Context) Fiber() *Fiber { return c.fiber }

// Name returns the diagnostic name of this context.
func (c *Context) Name() string { return c.name }

// Fork returns a child context that shares this context's fiber.
//
// It is the Go equivalent of Cordis's ctx.extend(): the child sees the same
// services and the same effect scope, but carries its own isolate map and name.
func (c *Context) Fork(name string) *Context {
	return &Context{shared: c.shared, parent: c, fiber: c.fiber, name: name}
}

// Isolate returns a child context in which service name resolves in a fresh
// scope. A service provided below the returned context is invisible to the
// parent scope and to siblings, which is how two plugins can each own their own
// "db" without colliding.
func (c *Context) Isolate(name string) *Context {
	return c.IsolateShared(name, c.shared.nextScope(name))
}

// IsolateShared is Isolate with an explicit scope label: two contexts isolated
// with the same name and label join one scope, mirroring Cordis's
// isolate(name, label).
func (c *Context) IsolateShared(name, label string) *Context {
	isolatedCtx := c.Fork(c.name)
	isolatedCtx.isolate = map[string]string{name: label}
	return isolatedCtx
}

// isolateLabel resolves the scope label of a service name by walking the
// context chain, defaulting to the service name itself.
func (c *Context) isolateLabel(name string) string {
	for cur := c; cur != nil; cur = cur.parent {
		if cur.isolate == nil {
			continue
		}
		if label, ok := cur.isolate[name]; ok {
			return label
		}
	}
	return name
}

func (c *core) nextScope(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scopeSeq++
	return fmt.Sprintf("%s#%d", name, c.scopeSeq)
}

// Done is closed when this context's fiber is disposed.
func (c *Context) Done() <-chan struct{} { return c.fiber.done }

// Context returns a context.Context that is cancelled when this context's
// fiber is disposed. Hand it to goroutines started by a plugin so that they
// stop when the plugin unloads.
func (c *Context) Context() context.Context { return c.fiber.lifecycleCtx }

// OnDispose registers a disposer owned by this context's fiber. Disposers run
// in reverse registration order when the fiber unloads. It panics with
// INACTIVE_EFFECT when the fiber is already disposed or unloading, matching
// Cordis's assertActive contract.
func (c *Context) OnDispose(fn func()) Disposer {
	return c.fiber.onDispose(fn)
}

// Effect runs body immediately and returns an idempotent Disposer that tears
// down whatever the body registered, mirroring ctx.effect() in Cordis.
func (c *Context) Effect(label string, body func() Disposer) Disposer {
	return c.fiber.effect(label, body)
}

// Effects returns the live effect metadata of this context's fiber.
func (c *Context) Effects() []*EffectMeta { return c.fiber.Effects() }

// Logger returns a named logger owned by the application.
func (c *Context) Logger(name ...string) *Logger {
	label := c.fiber.Name()
	if len(name) > 0 && name[0] != "" {
		label = name[0]
	}
	return &Logger{name: label, svc: c.shared.log}
}

// Provide registers a service under name, owned by this context's fiber.
func (c *Context) Provide(name string, service any) (Disposer, error) {
	return provide(c, name, service, nil)
}

// ProvideChecked registers a service together with an availability predicate.
// While availabilityCheck returns false, dependents treat the service as missing.
func (c *Context) ProvideChecked(name string, service any,
	availabilityCheck func() bool) (Disposer, error) {
	return provide(c, name, service, availabilityCheck)
}

// Set replaces a service this context's fiber owns.
func (c *Context) Set(name string, service any) error {
	return setService(c, name, service)
}

// Lookup reads a service without a type assertion. The second result is false
// when the service is unregistered or its provider is not active.
func (c *Context) Lookup(name string) (any, bool) {
	binding := c.resolveService(name)
	if binding == nil {
		return nil, false
	}
	return binding.service, true
}

// resolveService mirrors Cordis's context proxy lookup: walk the owning fiber's
// dependency snapshot upwards until the isolation scope changes, then fall back
// to the live service registry. The snapshot makes a service visible to its own
// provider and pins a dependent to the provider it loaded against.
func (c *Context) resolveService(name string) *serviceBinding {
	scopeLabel := c.isolateLabel(name)
	for fiber := c.fiber; fiber != nil; {
		fiber.mu.Lock()
		binding := fiber.resolvedServices[name]
		fiber.mu.Unlock()
		// The snapshot is keyed by service name, so it can hold at most one
		// binding per name; the scope guard keeps isolated services apart, and
		// the availability check hides a service while its predicate fails.
		if binding != nil && binding.scopeLabel == scopeLabel && binding.available() {
			return binding
		}
		parentCtx := fiber.Parent
		if parentCtx == nil || parentCtx.fiber == fiber {
			break
		}
		if parentCtx.fiber.Ctx.isolateLabel(name) != scopeLabel {
			break
		}
		fiber = parentCtx.fiber
	}
	return c.shared.lookupService(scopeLabel)
}

// Get reads a service as an untyped value.
func (c *Context) Get(name string) (any, bool) { return c.Lookup(name) }

// Get reads a service with a type assertion. It reports false when the service
// is missing, its provider is inactive, or the service has another type.
func Get[T any](c *Context, name string) (T, bool) {
	var zero T
	binding := c.resolveService(name)
	if binding == nil {
		return zero, false
	}
	service, ok := binding.service.(T)
	if !ok {
		return zero, false
	}
	return service, true
}

// MustGet reads a required service by name and panics with a diagnostic when it
// is unavailable. Inside a plugin, prefer declaring the dependency in Inject so
// the plugin waits instead of failing.
func MustGet[T any](c *Context, name string) T {
	service, ok := Get[T](c, name)
	if !ok {
		panic(newError(ErrServiceMissing, "required service %q is not available in context %q",
			name, c.name))
	}
	return service
}

// Provide registers a typed service owned by c's fiber.
func Provide[T any](c *Context, name string, service T) (Disposer, error) {
	return provide(c, name, service, nil)
}

// ProvideChecked registers a typed service with an availability predicate.
func ProvideChecked[T any](c *Context, name string, service T,
	availabilityCheck func() bool) (Disposer, error) {
	return provide(c, name, service, availabilityCheck)
}

// Serve registers a service and, when it implements Starter, calls Start after
// registration; a failed Start rolls the registration back. On dispose it calls
// Stop (if implemented) before unregistering, because Stop is registered later
// and disposers run in reverse order.
func Serve[T any](c *Context, name string, service T) (T, error) {
	disposer, err := provide(c, name, service, nil)
	if err != nil {
		return service, err
	}
	if starter, ok := any(service).(Starter); ok {
		if err := starter.Start(); err != nil {
			disposer()
			return service, err
		}
	}
	if stopper, ok := any(service).(Stopper); ok {
		c.OnDispose(func() {
			if err := stopper.Stop(); err != nil {
				c.Logger().Error("service %s: Stop failed: %v", name, err)
			}
		})
	}
	return service, nil
}

// Starter is implemented by services that need to run setup after registration.
type Starter interface{ Start() error }

// Stopper is implemented by services that need to release resources when the
// owning fiber unloads.
type Stopper interface{ Stop() error }

// EventService is the event bus exposed as the built-in "events" service.
type EventService struct {
	ctx *Context
}

// Context returns the context that owns the event service.
func (e *EventService) Context() *Context { return e.ctx }

// LoggerService is the logger factory exposed as the built-in "logger" service.
type LoggerService struct {
	svc *loggerService
}

// Logger returns a named logger.
func (l *LoggerService) Logger(name string) *Logger { return &Logger{name: name, svc: l.svc} }

// Registry is the public, read-only view of the plugin registry installed as
// the built-in "registry" service.
type Registry struct {
	shared *core
}

// Size reports how many plugin definitions have live fibers.
func (r *Registry) Size() int {
	r.shared.mu.Lock()
	defer r.shared.mu.Unlock()
	return len(r.shared.runtimes)
}

// Plugins returns the names of every plugin definition with live fibers.
func (r *Registry) Plugins() []string {
	r.shared.mu.Lock()
	defer r.shared.mu.Unlock()
	names := make([]string, 0, len(r.shared.runtimes))
	for def := range r.shared.runtimes {
		names = append(names, def.PluginName())
	}
	return names
}

func (c *core) registryFacade() Registry { return Registry{shared: c} }
