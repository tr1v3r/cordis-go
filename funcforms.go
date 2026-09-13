package cordis

// Package-level function forms of the Context methods.
//
// Upstream Cordis (TypeScript) exposes these operations only as methods on
// Context — there is no cordis.emit(ctx, ...) there. The functions below exist
// for callers that need the helper itself as a value of type func(*Context, ...):
// a method value such as ctx.Emit[T] has its receiver bound and carries no
// context parameter. Each function is otherwise identical to the Context method
// of the same name.

// On is the function form of Context.On.
func On[E any](c *Context, name string, fn func(E), opts ...EventOption) Disposer {
	return c.On(name, fn, opts...)
}

// OnOnce is the function form of Context.OnOnce.
func OnOnce[E any](c *Context, name string, fn func(E), opts ...EventOption) Disposer {
	return c.OnOnce(name, fn, opts...)
}

// OnValue is the function form of Context.OnValue.
func OnValue[E any](c *Context, name string, fn func(E) any, opts ...EventOption) Disposer {
	return c.OnValue(name, fn, opts...)
}

// OnWaterfall is the function form of Context.OnWaterfall.
func OnWaterfall[E any](c *Context, name string, fn func(E, func(E) any) any,
	opts ...EventOption) Disposer {
	return c.OnWaterfall(name, fn, opts...)
}

// Emit is the function form of Context.Emit.
func Emit[E any](c *Context, name string, payload E) {
	c.Emit(name, payload)
}

// EmitScoped is the function form of Context.EmitScoped.
func EmitScoped[E any](c *Context, scopeName, name string, payload E) {
	c.EmitScoped(scopeName, name, payload)
}

// Bail is the function form of Context.Bail.
func Bail[E any](c *Context, name string, payload E) (any, bool) {
	return c.Bail(name, payload)
}

// BailScoped is the function form of Context.BailScoped.
func BailScoped[E any](c *Context, scopeName, name string, payload E) (any, bool) {
	return c.BailScoped(scopeName, name, payload)
}

// Serial is the function form of Context.Serial.
func Serial[E any](c *Context, name string, payload E) (any, bool) {
	return c.Serial(name, payload)
}

// SerialScoped is the function form of Context.SerialScoped.
func SerialScoped[E any](c *Context, scopeName, name string, payload E) (any, bool) {
	return c.SerialScoped(scopeName, name, payload)
}

// Parallel is the function form of Context.Parallel.
func Parallel[E any](c *Context, name string, payload E) error {
	return c.Parallel(name, payload)
}

// ParallelScoped is the function form of Context.ParallelScoped.
func ParallelScoped[E any](c *Context, scopeName, name string, payload E) error {
	return c.ParallelScoped(scopeName, name, payload)
}

// Waterfall is the function form of Context.Waterfall.
func Waterfall[E any](c *Context, name string, payload E, final func(E) any) any {
	return c.Waterfall(name, payload, final)
}

// WaterfallScoped is the function form of Context.WaterfallScoped.
func WaterfallScoped[E any](c *Context, scopeName, name string, payload E, final func(E) any) any {
	return c.WaterfallScoped(scopeName, name, payload, final)
}

// Get is the function form of Context.Get.
func Get[T any](c *Context, name string) (T, bool) {
	return c.Get[T](name)
}

// MustGet is the function form of Context.MustGet.
func MustGet[T any](c *Context, name string) T {
	return c.MustGet[T](name)
}

// Provide is the function form of Context.Provide.
func Provide[T any](c *Context, name string, service T) (Disposer, error) {
	return c.Provide(name, service)
}

// ProvideChecked is the function form of Context.ProvideChecked.
func ProvideChecked[T any](c *Context, name string, service T,
	availabilityCheck func() bool) (Disposer, error) {
	return c.ProvideChecked(name, service, availabilityCheck)
}

// Serve is the function form of Context.Serve.
func Serve[T any](c *Context, name string, service T) (T, error) {
	return c.Serve(name, service)
}

// Load is the function form of Context.Load.
func Load[C any](parentCtx *Context, plugin *Plugin[C], config C) (*Fiber, error) {
	return parentCtx.Load(plugin, config)
}

// LoadWithInject is the function form of Context.LoadWithInject.
func LoadWithInject[C any](parentCtx *Context, plugin *Plugin[C], config C,
	extra ...string) (*Fiber, error) {
	return parentCtx.LoadWithInject(plugin, config, extra...)
}
