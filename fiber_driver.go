package cordis

// This file owns the per-fiber transition driver. The driver gives one caller
// ownership of a transition pass while other callers record that another pass
// is needed instead of starting a second one.

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

// claimDispose marks the fiber disposed. It reports whether a transition owner
// is running and whether another caller already claimed the disposal. The
// dirty flag is set in the same critical section so a running owner cannot
// clear busy and exit before seeing the request.
func (f *Fiber) claimDispose() (busy, already bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disposed {
		return false, true
	}
	f.disposed = true
	if f.busy {
		f.dirty = true
		return true, false
	}
	return false, false
}

// takeRequest consumes pending transition requests under the fiber lock. It
// reports whether the fiber is disposed and whether a forced reload is pending.
func (f *Fiber) takeRequest() (disposed, forceReload bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	disposed = f.disposed
	forceReload = f.forceReload
	f.forceReload = false
	return
}
