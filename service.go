package cordis

import (
	"fmt"
	"sync"
)

// serviceBinding associates a service name and scope with its provider and
// concrete service object.
type serviceBinding struct {
	name              string
	scopeLabel        string
	provider          *Fiber
	service           any
	availabilityCheck func() bool

	mu sync.RWMutex
}

// getService returns the current service value.
func (b *serviceBinding) getService() any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.service
}

// setService replaces the current service value.
func (b *serviceBinding) setService(service any) {
	b.mu.Lock()
	b.service = service
	b.mu.Unlock()
}

func provide(c *Context, name string, service any,
	availabilityCheck func() bool) (Disposer, error) {
	if name == "" {
		return nil, newError(ErrServiceMissing, "service name must not be empty")
	}
	ownerFiber := c.fiber
	scopeLabel := c.isolateLabel(name)
	binding := &serviceBinding{
		name:              name,
		scopeLabel:        scopeLabel,
		provider:          ownerFiber,
		service:           service,
		availabilityCheck: availabilityCheck,
	}

	// Report a dead owner as a typed error rather than panicking out of a
	// constructor, which is where Provide is normally called.
	ownerFiber.mu.Lock()
	inactive := ownerFiber.disposed || ownerFiber.state == StateUnloading
	ownerFiber.mu.Unlock()
	if inactive {
		return nil, newError(ErrInactiveEffect,
			"cannot provide service %q on inactive context %q", name, ownerFiber.Name())
	}
	if err := c.shared.registerService(binding); err != nil {
		return nil, err
	}

	disposer, err := ownerFiber.tryEffect(fmt.Sprintf("ctx.Provide(%q)", name), func() Disposer {
		// A service is visible to its own provider immediately, so a plugin may
		// call the service it provides. Pending fibers have no resolved services
		// yet; their snapshot is built when they load.
		ownerFiber.mu.Lock()
		if ownerFiber.resolvedServices != nil {
			ownerFiber.resolvedServices[name] = binding
		}
		ownerFiber.mu.Unlock()

		if ownerFiber.State() == StateActive {
			c.shared.notify(name, scopeLabel)
		}

		return func() {
			c.shared.unregisterService(binding)
			c.shared.notify(name, scopeLabel)
			ownerFiber.mu.Lock()
			delete(ownerFiber.resolvedServices, name)
			ownerFiber.mu.Unlock()
		}
	})
	if err != nil {
		c.shared.unregisterService(binding)
		return nil, err
	}
	return disposer, nil
}

func setService(c *Context, name string, service any) error {
	scopeLabel := c.isolateLabel(name)
	binding := c.shared.getServiceBinding(scopeLabel)
	// A binding registered under another name is not this service, so a Set for
	// an unknown name must not overwrite the service that shares its label.
	if binding == nil || binding.name != name {
		return newError(ErrServiceMissing, "cannot set service %q before it is provided", name)
	}
	if binding.provider != c.fiber {
		return newError(ErrServiceOwnership, "cannot set service %q from another fiber", name)
	}
	if c.shared.updateService(binding, service) {
		c.shared.notify(name, scopeLabel)
		return nil
	}
	return newError(ErrServiceMissing,
		"cannot set service %q: the provider changed while updating", name)
}

// updateService replaces the value of binding only while it is still the
// registered binding for its scope. It reports false when another provider
// replaced the binding between lookup and update.
func (c *core) updateService(binding *serviceBinding, service any) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.serviceBindings[binding.scopeLabel]
	if current == binding {
		binding.setService(service)
		return true
	}
	return false
}

func (c *core) registerService(binding *serviceBinding) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.serviceBindings[binding.scopeLabel]; ok {
		owner := "unknown"
		if existing.provider != nil {
			owner = existing.provider.Name()
		}
		return newError(ErrServiceExists,
			"service %q is already provided by <%s>", binding.name, owner)
	}
	c.serviceBindings[binding.scopeLabel] = binding
	return nil
}

func (c *core) unregisterService(binding *serviceBinding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.serviceBindings[binding.scopeLabel]; ok && current == binding {
		delete(c.serviceBindings, binding.scopeLabel)
	}
}

func (c *core) getServiceBinding(scopeLabel string) *serviceBinding {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serviceBindings[scopeLabel]
}

// lookupService returns the binding registered for name in scopeLabel, but only
// while its provider is active and its availability predicate, if any, passes.
// A label carries one service name, so a binding registered under another name
// is not this service: reporting it would alias every name isolated onto that
// label onto a single service.
func (c *core) lookupService(scopeLabel, name string) *serviceBinding {
	binding := c.getServiceBinding(scopeLabel)
	if binding == nil || binding.name != name {
		return nil
	}
	if binding.provider != nil && binding.provider.State() != StateActive {
		return nil
	}
	if !binding.available() {
		return nil
	}
	return binding
}

func (b *serviceBinding) available() bool {
	return b.availabilityCheck == nil || runCheck(b.availabilityCheck)
}

// live reports whether the binding still resolves to an active provider.
func (b *serviceBinding) live() bool {
	return b.provider != nil && b.provider.State() == StateActive && b.available()
}

func runCheck(check func() bool) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return check()
}

// providedNames lists the services a fiber currently owns.
func (c *core) providedNames(fiber *Fiber) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var names []string
	for _, binding := range c.serviceBindings {
		if binding.provider == fiber {
			names = append(names, binding.name)
		}
	}
	return names
}

// notify re-evaluates every fiber that requires name in the given scope.
func (c *core) notify(name, scope string) {
	var targets []*Fiber
	for _, fiber := range c.snapshotFibers() {
		if fiber.injects(name) && fiber.Ctx.isolateLabel(name) == scope {
			targets = append(targets, fiber)
		}
	}
	for _, fiber := range targets {
		fiber.refresh()
	}
}
