package cordis

import (
	"slices"
	"sync"
)

// Disposer releases a resource. Disposers produced by this package are
// idempotent and safe to call from multiple goroutines. Cleanup functions must
// not create a cycle of Disposer calls, such as calling their own or an ancestor's
// Disposer or having two sibling cleanups call each other; like recursive
// sync.Once.Do use, such cycles deadlock.
type Disposer func()

// Once wraps fn into a Disposer that runs at most once. A nil fn yields a
// no-op Disposer, so callers never need a nil check.
func Once(fn func()) Disposer {
	var once sync.Once
	return func() {
		if fn == nil {
			return
		}
		once.Do(fn)
	}
}

// EffectMeta describes a live effect for diagnostics, mirroring Cordis's
// Fiber#getEffects(). Effects registered through the Context passed to this
// effect's body are exposed through Children.
type EffectMeta struct {
	Label string

	mu       sync.Mutex
	children []*EffectMeta
}

// Children returns a snapshot of the nested effect metadata.
func (m *EffectMeta) Children() []*EffectMeta {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*EffectMeta, len(m.children))
	copy(out, m.children)
	return out
}

func (m *EffectMeta) addChild(child *EffectMeta) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.children = append(m.children, child)
}

func (m *EffectMeta) removeChild(child *EffectMeta) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, candidate := range m.children {
		if candidate == child {
			m.children = append(m.children[:i], m.children[i+1:]...)
			return
		}
	}
}

// effectEntry is one registered effect inside a fiber.
//
// The disposer is installed only after the effect body returns, so the entry can
// be published before the body runs: a reentrant or concurrent unload then sees
// it and defers the teardown instead of orphaning it.
type effectEntry struct {
	meta *EffectMeta

	mu          sync.Mutex
	dispose     Disposer
	deferred    bool
	done        bool
	releaseOnce sync.Once
	// running marks the entry whose body is executing right now. Only a running
	// entry may adopt nested effects: nesting under a finished one would leave
	// the child where no unload can reach it.
	running bool
	// children are effects registered through the explicit context passed to
	// this effect's body. Their lifetime belongs to this effect, mirroring
	// Cordis's effect collector.
	children []*effectEntry
}

// adopt takes ownership of an effect registered through this entry's explicit
// context while its body runs. It reports false once that scope is inactive.
func (e *effectEntry) adopt(child *effectEntry) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done || !e.running {
		return false
	}
	e.children = append(e.children, child)
	e.meta.addChild(child.meta)
	return true
}

// finishBody marks this entry's explicit scope inactive.
func (e *effectEntry) finishBody() {
	e.mu.Lock()
	e.running = false
	e.mu.Unlock()
}

// detach releases a nested effect that was disposed individually.
func (e *effectEntry) detach(child *effectEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, candidate := range e.children {
		if candidate == child {
			e.children = append(e.children[:i], e.children[i+1:]...)
			break
		}
	}
	e.meta.removeChild(child.meta)
}

// requestDispose reports whether the disposer is installed. A request that
// arrives while the effect body runs is deferred until setDispose installs it.
func (e *effectEntry) requestDispose() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dispose == nil {
		if !e.done {
			e.deferred = true
		}
		return false
	}
	return true
}

// claimDispose marks the entry claimed by releaseOnce and extracts its disposer
// and children. The shared once blocks every concurrent handle until both have
// finished unwinding.
func (e *effectEntry) claimDispose() (Disposer, []*effectEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return nil, nil
	}
	e.done = true
	children := e.children
	e.children = nil
	return e.dispose, children
}

// setDispose installs the body's disposer and reports whether disposal was
// requested while the body ran, meaning the caller must run it now.
func (e *effectEntry) setDispose(dispose Disposer) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dispose = dispose
	return e.deferred && !e.done
}

// abandon marks the entry as finished without ever running a disposer, for an
// effect body that panicked before producing one. It returns the nested effects
// the entry had adopted, so the caller can still unwind them: abandoning the
// entry must not make its children unreachable.
func (e *effectEntry) abandon() []*effectEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.done = true
	e.running = false
	children := e.children
	e.children = nil
	return children
}

// disposableList is an ordered collection of effects supporting removal by
// handle and reverse-order teardown.
type disposableList struct {
	mu    sync.Mutex
	seq   int
	items map[int]*effectEntry
}

func newDisposableList() *disposableList {
	return &disposableList{items: map[int]*effectEntry{}}
}

// add stores entry and returns a removal handle.
func (l *disposableList) add(entry *effectEntry) (int, func() bool) {
	l.mu.Lock()
	l.seq++
	handle := l.seq
	l.items[handle] = entry
	l.mu.Unlock()
	return handle, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		if _, ok := l.items[handle]; !ok {
			return false
		}
		delete(l.items, handle)
		return true
	}
}

// snapshot returns the entries in registration order without clearing them,
// matching Cordis's getEffects().
func (l *disposableList) snapshot() []*effectEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshotLocked()
}

// snapshotLocked returns the entries in registration order. The caller must
// hold l.mu.
func (l *disposableList) snapshotLocked() []*effectEntry {
	handles := make([]int, 0, len(l.items))
	for handle := range l.items {
		handles = append(handles, handle)
	}
	slices.Sort(handles)
	entries := make([]*effectEntry, 0, len(handles))
	for _, handle := range handles {
		entries = append(entries, l.items[handle])
	}
	return entries
}

// clear empties the list and returns the entries newest-first, which is the
// order Cordis uses to unwind a fiber.
func (l *disposableList) clear() []*effectEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.snapshotLocked()
	slices.Reverse(entries)
	l.items = make(map[int]*effectEntry)
	return entries
}
