package catalog

// window.go is the catalog's run-window face: the read/write split a run opens before it
// starts writing media (see docs/design/catalog-window.md). OpenWindow returns a View — a
// point-in-time, read-only copy of every run's placements for the window's readers — and a
// Window — the handle that marks the run as the catalog's single writer until Close.
//
// The window changes no durability or mutation semantics: every mutator persists per op
// (atomic tmp+rename) exactly as outside a window, so every committed archive is durable
// the moment it records — the archive is the commit unit, and there is no run-level
// rollback. All mutation runs on the run's single writer goroutine (the spool orchestrator,
// plus the librarians it drives); readers never see mid-window state because they read the
// View's copy. The cross-process case is excluded by the run lock, not by the window.
// (A journaled batch-commit was considered and dropped: per-op persist is already atomic
// and durable, and the catalog is small — see the design doc.)

import "errors"

// View is a point-in-time, read-only copy of every run's placements, taken at window-open
// for the window's readers. It is a deep copy down to each archive's part list, so a
// reader never shares an array with the live entries the window's writer mutates. Serving
// reads from a copy is sound because a session never reads its own writes through the
// catalog (see docs/design/catalog-window.md).
type View struct {
	placements map[string][]Placement
}

// PlacementsFor returns the copies of a run as of window-open.
func (v *View) PlacementsFor(runID string) []Placement { return v.placements[runID] }

// Window marks an open run window. It is not goroutine-safe: the run's single writer
// goroutine owns it, like the catalog itself.
type Window struct {
	c *Catalog
}

// OpenWindow starts a run window: the window's readers work from the View's copy while
// the run mutates the catalog. One window at a time.
func (c *Catalog) OpenWindow() (*View, *Window, error) {
	if c.win != nil {
		return nil, nil, errors.New("catalog window already open")
	}
	w := &Window{c: c}
	c.win = w
	return &View{placements: c.snapshotPlacements()}, w, nil
}

// Close ends the window. It runs unconditionally at window end — success, producer error,
// or cancel — every archive recorded before it is already persisted.
func (w *Window) Close() error {
	if w.c == nil {
		return errors.New("catalog window already closed")
	}
	w.c.win = nil
	w.c = nil
	return nil
}
