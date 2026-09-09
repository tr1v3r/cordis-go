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
	fiber := c.fiber
	scope := c.isolateLabel(name)
	entry := &impl{name: name, scope: scope, fiber: fiber, value: value, check: check}

	// Report a dead owner as a typed error rather than panicking out of a
	// constructor, which is where Provide is normally called.
	fiber.mu.Lock()
	inactive := fiber.disposed || fiber.state == StateUnloading
	fiber.mu.Unlock()
	if inactive {
		return nil, newError(ErrInactiveEffect, "cannot provide service %q on inactive context %q", name, fiber.Name())
	}
	if err := c.shared.registerImpl(entry); err != nil {
		return nil, err
	}

	disposer, err := fiber.tryEffect(fmt.Sprintf("ctx.Provide(%q)", name), func() Disposer {
		// A service is visible to its own provider immediately, so a plugin may
		// call the service it provides. Pending fibers have no store yet; their
		// snapshot is built when they load.
		fiber.mu.Lock()
		if fiber.store != nil {
			fiber.store[name] = entry
		}
		fiber.mu.Unlock()

		if fiber.State() == StateActive {
			c.shared.notify(name, scope)
		}

		return func() {
			c.shared.unregisterImpl(entry)
			c.shared.notify(name, scope)
			fiber.mu.Lock()
			delete(fiber.store, name)
			fiber.mu.Unlock()
		}
	})
	if err != nil {
		c.shared.unregisterImpl(entry)
		return nil, err
	}
	return disposer, nil
}

func setService(c *Context, name string, value any) error {
	scope := c.isolateLabel(name)
	entry := c.shared.getImpl(scope)
	if entry == nil {
		return newError(ErrServiceMissing, "cannot set service %q before it is provided", name)
	}
	if entry.fiber != c.fiber {
		return newError(ErrServiceOwnership, "cannot set service %q from another fiber", name)
	}
	c.shared.mu.Lock()
	entry.value = value
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
	entry := c.getImpl(scope)
	if entry == nil {
		return nil
	}
	if entry.fiber != nil && entry.fiber.State() != StateActive {
		return nil
	}
	if !entry.available() {
		return nil
	}
	return entry
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
	for _, entry := range c.store {
		if entry.fiber == fiber {
			names = append(names, entry.name)
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
