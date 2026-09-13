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
		f.beginPass()
		f.sync()
		if f.endPass() {
			continue
		}
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

// beginPass clears the rerun marker for one transition pass.
func (f *Fiber) beginPass() {
	f.mu.Lock()
	f.dirty = false
	f.mu.Unlock()
}

// endPass releases ownership unless another pass was requested. It reports
// whether the owner loop must continue.
func (f *Fiber) endPass() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirty {
		return true
	}
	f.busy = false
	return false
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

// transitionRequest captures the volatile requests observed at the start of a
// transition pass.
type transitionRequest struct {
	disposed    bool
	forceReload bool
}

// takeRequest consumes pending transition requests under the fiber lock.
func (f *Fiber) takeRequest() transitionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	req := transitionRequest{
		disposed:    f.disposed,
		forceReload: f.forceReload,
	}
	f.forceReload = false
	return req
}
