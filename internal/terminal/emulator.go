package terminal

import "github.com/charmbracelet/x/vt"

// Emulator is the narrow slice of vt.Emulator's interface this package
// actually uses. Defined locally so tests can substitute a fake.
//
// ScrollbackLen/ScrollbackLine expose vt.Emulator's own scrollback buffer
// (it already maintains one internally — see vt.Scrollback — this package
// just never read from it before) as plain ints/strings rather than the
// vt/uv package's own Scrollback/Line types, so this interface — and every
// fake substituting for it — stays free of that dependency, matching how
// Render() already returns a plain string rather than a vt-specific type.
type Emulator interface {
	Write(p []byte) (int, error)
	Render() string
	Resize(width, height int)
	// ScrollbackLen returns the number of lines currently held in the
	// scrollback buffer (lines that have scrolled off the top of the live
	// screen), oldest-first.
	ScrollbackLen() int
	// ScrollbackLine returns the rendered (ANSI-styled) text of the
	// scrollback line at index (0 = oldest), or "" if index is out of
	// range.
	ScrollbackLine(index int) string
	// IsAltScreen reports whether the terminal is currently showing its
	// alternate screen (full-screen apps like vim/htop/less switch to
	// this while running). The alt screen doesn't use the main screen's
	// scrollback buffer — ScrollbackLen/ScrollbackLine here would return
	// unrelated, stale content from before the app started — so callers
	// must treat "alt screen active" as "no scrollback available" rather
	// than blending the two.
	IsAltScreen() bool
	// CursorPosition returns the cursor's current (column, row), both
	// 0-indexed. Exposed as plain ints rather than vt/uv's own Position
	// type (a plain image.Point) for the same reason as
	// ScrollbackLen/ScrollbackLine above — reflow needs it to map the
	// cursor through a width change (see reflowRows and cursorOffset in
	// reflow.go): the cursor's physical (row, column) means something
	// different at every width, so reflow converts it into an offset
	// within its own logical line before rewrapping and back into
	// physical coordinates afterward, which is impossible without
	// knowing where it started.
	CursorPosition() (x, y int)
}

// vtEmulator adapts *vt.Emulator to this package's narrower Emulator
// interface — vt.Emulator itself has no ScrollbackLine(index) string
// method (its Scrollback() returns a *vt.Scrollback of vt/uv-typed Lines,
// not a plain string), so this wraps it rather than returning *vt.Emulator
// directly from newEmulator. ScrollbackLen and IsAltScreen need no
// forwarding method of their own — *vt.Emulator already has matching
// signatures for both, promoted automatically through the embedded field.
type vtEmulator struct {
	*vt.Emulator
}

func (e vtEmulator) ScrollbackLine(index int) string {
	line := e.Emulator.Scrollback().Line(index)
	if line == nil {
		return ""
	}
	return line.Render()
}

func (e vtEmulator) CursorPosition() (x, y int) {
	pos := e.Emulator.CursorPosition()
	return pos.X, pos.Y
}

// newEmulator is a package-level var so tests can substitute a fake
// constructor.
var newEmulator = func(width, height int) Emulator {
	return vtEmulator{vt.NewEmulator(width, height)}
}
