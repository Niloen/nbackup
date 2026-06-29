// Package logf is the shared progress-logger seam. It lives at the bottom of the
// import graph — depending on nothing — so the engine and the packages split out
// of it (accounting, scheduler, conductor) can all take a Logf without forming an
// import cycle through the engine.
package logf

// Logf is an optional progress logger. A nil Logf is a no-op.
type Logf func(format string, args ...any)

// Log emits one progress line, ignoring a nil logger so callers never guard it.
func (l Logf) Log(format string, args ...any) {
	if l != nil {
		l(format, args...)
	}
}
