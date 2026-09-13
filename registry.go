package cordis

import (
	"fmt"
	"reflect"
)

// definition is the contract every Plugin[C] implements, and the erasure
// boundary the runtime works through: a fiber reaches the plugin body only
// through ResolveConfig and Run, without knowing its config type.
//
// It is deliberately unexported. Typed callers go through Context.Load, which
// checks the config type at compile time; a host whose plugin types are only
// known at run time erases the config type by capturing it in a closure at
// registration time instead (see the loader package).
//
// Implementations must be comparable pointers: the registry keys plugin
// runtimes by definition identity, so loading one definition twice creates two
// fibers under a single plugin runtime.
type definition interface {
	// PluginName is the diagnostic name of the plugin.
	PluginName() string
	// InjectKeys lists the services the plugin requires before it may load.
	InjectKeys() []string
	// ResolveConfig validates and converts the raw config.
	ResolveConfig(raw any) (config any, err error)
	// Run executes the plugin body.
	Run(ctx *Context, config any) error
}

// runtime is the per-definition record shared by every live instance of
// one plugin.
type runtime struct {
	name       string
	definition definition
	fibers     []*Fiber
}

// Plugin is a typed plugin definition.
//
// Config is any Go value; the loader decodes JSON into it, and direct callers
// pass it to Load. Validate runs before every activation.
type Plugin[C any] struct {
	Name     string
	Inject   []string
	Validate func(*C) error
	Apply    func(ctx *Context, config C) error
}

// Define creates a plugin from a name and a body.
func Define[C any](name string, apply func(ctx *Context, config C) error) *Plugin[C] {
	return &Plugin[C]{Name: name, Apply: apply}
}

// WithInject declares required services and returns the plugin for chaining.
func (p *Plugin[C]) WithInject(names ...string) *Plugin[C] {
	p.Inject = append(p.Inject, names...)
	return p
}

// WithValidate attaches a config validator and returns the plugin.
func (p *Plugin[C]) WithValidate(validate func(*C) error) *Plugin[C] {
	p.Validate = validate
	return p
}

// PluginName reports the diagnostic name of the plugin.
func (p *Plugin[C]) PluginName() string {
	if p.Name == "" {
		return "anonymous"
	}
	return p.Name
}

// InjectKeys lists the services the plugin requires before it may load.
func (p *Plugin[C]) InjectKeys() []string { return p.Inject }

// ResolveConfig validates the raw config and converts it to this plugin's
// config type. A config of another type is rejected.
func (p *Plugin[C]) ResolveConfig(raw any) (any, error) {
	var config C
	switch value := raw.(type) {
	case nil:
		// keep the zero config
	case C:
		config = value
	default:
		return nil, newError(ErrInvalidPlugin, "plugin %s: config has type %T, want %T",
			p.PluginName(), raw, config)
	}
	if p.Validate != nil {
		if err := p.Validate(&config); err != nil {
			return nil, fmt.Errorf("plugin %s: invalid config: %w", p.PluginName(), err)
		}
	}
	return config, nil
}

// Run executes the plugin body with the config produced by ResolveConfig.
func (p *Plugin[C]) Run(ctx *Context, config any) error {
	if p.Apply == nil {
		return newError(ErrInvalidPlugin, "plugin %s has no Apply function", p.PluginName())
	}
	value, _ := config.(C)
	return p.Apply(ctx, value)
}

// Load starts a typed plugin in this context and returns its fiber.
//
// Loading is synchronous, so when Load returns the fiber has already settled
// into active, pending (dependencies unmet) or failed. A failed plugin body is
// reported as the returned error and stays readable through fiber.Error(); the
// fiber itself is still returned so the caller can inspect or restart it. The
// plugin stays pending until every service it declares in Inject is provided by
// an active fiber.
func (c *Context) Load[C any](plugin *Plugin[C], config C) (*Fiber, error) {
	return load(c, plugin, config, nil)
}

// LoadWithInject starts a typed plugin with extra required services on top of
// the ones the plugin declares itself. Loaders use it for config-driven inject.
func (c *Context) LoadWithInject[C any](plugin *Plugin[C], config C,
	extra ...string) (*Fiber, error) {
	return load(c, plugin, config, extra)
}

func load(parentCtx *Context, definition definition, config any, extra []string) (*Fiber, error) {
	if definition == nil {
		return nil, newError(ErrInvalidPlugin, "nil plugin definition")
	}
	if err := parentCtx.fiber.assertActive(); err != nil {
		return nil, err
	}

	inject := map[string]struct{}{}
	for _, name := range definition.InjectKeys() {
		if name != "" {
			inject[name] = struct{}{}
		}
	}
	for _, name := range extra {
		if name != "" {
			inject[name] = struct{}{}
		}
	}
	runtime, err := parentCtx.shared.runtimeFor(definition)
	if err != nil {
		return nil, err
	}
	fiber, err := newFiber(parentCtx, runtime, config, inject)
	if err != nil {
		return nil, err
	}
	if fiber.State() == StateFailed {
		// Cordis surfaces a startup error through fiber.await(); a synchronous
		// Load has no later await point, so it must return the error here or a
		// failing plugin body would look like a successful load.
		return fiber, fiber.Error()
	}
	return fiber, nil
}

func (c *core) runtimeFor(definition definition) (*runtime, error) {
	kind := reflect.TypeOf(definition)
	if kind == nil || !kind.Comparable() {
		return nil, newError(ErrInvalidPlugin,
			"plugin definition must be a comparable pointer, got %T", definition)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.runtimes[definition]; ok {
		return existing, nil
	}
	runtime := &runtime{name: definition.PluginName(), definition: definition}
	c.runtimes[definition] = runtime
	return runtime, nil
}

// injectDefinition backs Context.Inject.
type injectDefinition struct {
	name string
	deps []string
	body func(*Context) error
}

func (d *injectDefinition) PluginName() string             { return d.name }
func (d *injectDefinition) InjectKeys() []string           { return d.deps }
func (d *injectDefinition) ResolveConfig(any) (any, error) { return nil, nil }
func (d *injectDefinition) Run(ctx *Context, _ any) error  { return d.body(ctx) }

// Inject runs body once every service in deps is available, reloading it
// whenever a provider changes. It is the Go form of ctx.inject().
func Inject(parentCtx *Context, deps []string, body func(*Context) error) (*Fiber, error) {
	name := "inject"
	if body != nil {
		name = fmt.Sprintf("inject#%p", body)
	}
	return load(parentCtx, &injectDefinition{name: name, deps: deps, body: body}, nil, nil)
}
