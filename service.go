package cordis

import "fmt"

// impl is one provided service implementation, keyed by isolation scope label.
type impl struct {
	name  string
	scope string
	fiber *Fiber
	value any
	check func() bool
}

func provide(c *Context, name string, value any, check func() bool) (Disposer, error) {
	if name == "" {
		return nil, newError(ErrServiceMissing, "service name must not be empty")
	}
	ownerFiber := c.fiber
	scope := c.isolateLabel(name)
	serviceImpl := &impl{name: name, scope: scope, fiber: ownerFiber, value: value, check: check}

	// Report a dead owner as a typed error rather than panicking out of a
	// constructor, which is where Provide is normally called.
	ownerFiber.mu.Lock()
	inactive := ownerFiber.disposed || ownerFiber.state == StateUnloading
	ownerFiber.mu.Unlock()
	if inactive {
		return nil, newError(ErrInactiveEffect, "cannot provide service %q on inactive context %q", name, ownerFiber.Name())
	}
	if err := c.shared.registerImpl(serviceImpl); err != nil {
		return nil, err
	}

	disposer, err := ownerFiber.tryEffect(fmt.Sprintf("ctx.Provide(%q)", name), func() Disposer {
		// A service is visible to its own provider immediately, so a plugin may
		// call the service it provides. Pending fibers have no store yet; their
		// snapshot is built when they load.
		ownerFiber.mu.Lock()
		if ownerFiber.store != nil {
			ownerFiber.store[name] = serviceImpl
		}
		ownerFiber.mu.Unlock()

		if ownerFiber.State() == StateActive {
			c.shared.notify(name, scope)
		}

		return func() {
			c.shared.unregisterImpl(serviceImpl)
			c.shared.notify(name, scope)
			ownerFiber.mu.Lock()
			delete(ownerFiber.store, name)
			ownerFiber.mu.Unlock()
		}
	})
	if err != nil {
		c.shared.unregisterImpl(serviceImpl)
		return nil, err
	}
	return disposer, nil
}

func setService(c *Context, name string, value any) error {
	scope := c.isolateLabel(name)
	serviceImpl := c.shared.getImpl(scope)
	if serviceImpl == nil {
		return newError(ErrServiceMissing, "cannot set service %q before it is provided", name)
	}
	if serviceImpl.fiber != c.fiber {
		return newError(ErrServiceOwnership, "cannot set service %q from another fiber", name)
	}
	c.shared.mu.Lock()
	serviceImpl.value = value
	c.shared.mu.Unlock()
	c.shared.notify(name, scope)
	return nil
}

func (c *core) registerImpl(entry *impl) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.store[entry.scope]; ok {
		owner := "unknown"
		if existing.fiber != nil {
			owner = existing.fiber.Name()
		}
		return newError(ErrServiceExists, "service %q is already provided by <%s>", entry.name, owner)
	}
	c.store[entry.scope] = entry
	return nil
}

func (c *core) unregisterImpl(entry *impl) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.store[entry.scope]; ok && current == entry {
		delete(c.store, entry.scope)
	}
}

func (c *core) getImpl(scope string) *impl {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store[scope]
}

// lookupStrict resolves a service the way Cordis does with strict=true: the
// provider must be ACTIVE and its availability predicate (if any) must pass.
func (c *core) lookupStrict(name, scope string) *impl {
	serviceImpl := c.getImpl(scope)
	if serviceImpl == nil {
		return nil
	}
	if serviceImpl.fiber != nil && serviceImpl.fiber.State() != StateActive {
		return nil
	}
	if !serviceImpl.available() {
		return nil
	}
	return serviceImpl
}

// available reports whether the implementation's availability predicate passes.
func (i *impl) available() bool {
	return i.check == nil || runCheck(i.check)
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
	for _, serviceImpl := range c.store {
		if serviceImpl.fiber == fiber {
			names = append(names, serviceImpl.name)
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
