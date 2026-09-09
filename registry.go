package cordis

import (
	"fmt"
	"reflect"
)

// Definition is the runtime form of a plugin.
//
// Implementations must be comparable (use a pointer type), because the registry
// keys plugin runtimes by definition identity: loading the same definition
// twice creates two fibers under one runtime.
type Definition interface {
	// PluginName is the diagnostic name of the plugin.
	PluginName() string
	// InjectKeys lists the services the plugin requires before it may load.
	InjectKeys() []string
	// ResolveConfig validates and converts the raw config.
	ResolveConfig(raw any) (any, error)
	// Run executes the plugin body.
	Run(ctx *Context, config any) error
}

// runtime is the mutable record shared by every fiber of one plugin.
type runtime struct {
	name   string
	def    Definition
	fibers []*Fiber
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

// PluginName implements Definition.
func (p *Plugin[C]) PluginName() string {
	if p.Name == "" {
		return "anonymous"
	}
	return p.Name
}

// InjectKeys implements Definition.
func (p *Plugin[C]) InjectKeys() []string { return p.Inject }

// ResolveConfig implements Definition.
func (p *Plugin[C]) ResolveConfig(raw any) (any, error) {
	var config C
	switch value := raw.(type) {
	case nil:
		// keep the zero config
	case C:
		config = value
	default:
		return nil, newError(ErrInvalidPlugin, "plugin %s: config has type %T, want %T", p.PluginName(), raw, config)
	}
	if p.Validate != nil {
		if err := p.Validate(&config); err != nil {
			return nil, fmt.Errorf("plugin %s: invalid config: %w", p.PluginName(), err)
		}
	}
	return config, nil
}

// Run implements Definition.
func (p *Plugin[C]) Run(ctx *Context, config any) error {
	if p.Apply == nil {
		return newError(ErrInvalidPlugin, "plugin %s has no Apply function", p.PluginName())
	}
	value, _ := config.(C)
	return p.Apply(ctx, value)
}

// Load starts a typed plugin in the parent context.
func Load[C any](parent *Context, plugin *Plugin[C], config C) (*Fiber, error) {
	return load(parent, plugin, config, nil)
}

// LoadWithInject starts a typed plugin with extra required services.
func LoadWithInject[C any](parent *Context, plugin *Plugin[C], config C, extra ...string) (*Fiber, error) {
	return load(parent, plugin, config, extra)
}

func load(parent *Context, def Definition, config any, extra []string) (*Fiber, error) {
	if def == nil {
		return nil, newError(ErrInvalidPlugin, "nil plugin definition")
	}
	if err := parent.fiber.assertActive(); err != nil {
		return nil, err
	}

	inject := map[string]struct{}{}
	for _, name := range def.InjectKeys() {
		if name != "" {
			inject[name] = struct{}{}
		}
	}
	for _, name := range extra {
		if name != "" {
			inject[name] = struct{}{}
		}
	}
	rt, err := parent.shared.runtimeFor(def)
	if err != nil {
		return nil, err
	}
	return newFiber(parent, rt, config, inject), nil
}

func (c *core) runtimeFor(def Definition) (*runtime, error) {
	kind := reflect.TypeOf(def)
	if kind == nil || !kind.Comparable() {
		return nil, newError(ErrInvalidPlugin, "plugin definition must be a comparable pointer, got %T", def)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.plugins[def]; ok {
		return existing, nil
	}
	rt := &runtime{name: def.PluginName(), def: def}
	c.plugins[def] = rt
	return rt, nil
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
func Inject(parent *Context, deps []string, body func(*Context) error) (*Fiber, error) {
	name := "inject"
	if body != nil {
		name = fmt.Sprintf("inject#%p", body)
	}
	return load(parent, &injectDefinition{name: name, deps: deps, body: body}, nil, nil)
}
