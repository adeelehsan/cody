package terminal

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/vt"
)

type fakePty struct {
	writes   []byte
	toRead   [][]byte
	closed   bool
	resizeW  int
	resizeH  int
	resizes  int
	startErr error
	readErr  error
}

func (f *fakePty) Read(p []byte) (int, error) {
	if len(f.toRead) == 0 {
		if f.readErr != nil {
			return 0, f.readErr
		}
		return 0, io.EOF
	}
	chunk := f.toRead[0]
	f.toRead = f.toRead[1:]
	n := copy(p, chunk)
	return n, nil
}

func (f *fakePty) Write(p []byte) (int, error) {
	f.writes = append(f.writes, p...)
	return len(p), nil
}

func (f *fakePty) Close() error {
	f.closed = true
	return nil
}

func (f *fakePty) Resize(w, h int) error {
	f.resizeW, f.resizeH = w, h
	f.resizes++
	return nil
}

func (f *fakePty) Start(cmd *exec.Cmd) error {
	return f.startErr
}

type fakeEmulator struct {
	written []byte
	w, h    int
	resizes int
	// sbLines is test-controlled scrollback content, oldest first —
	// independent of written/Write, since real scrollback population is
	// the vt library's own internal concern (see Scrollback in
	// charmbracelet/x/vt), not something this package's Write forwards to
	// in the fake.
	sbLines []string
	// pushOnWrite, if set, is appended to sbLines the next time Write is
	// called (then cleared) — simulates a real Write pushing new lines
	// into scrollback as a side effect, so a test can control exactly
	// when that growth is observed relative to Update's own
	// before/after ScrollbackLen() measurement.
	pushOnWrite []string
	altScreen   bool
	// altScreenAfterWrite, if non-nil, sets altScreen to this value the
	// next time Write is called (then clears itself) — simulates a real
	// Write transitioning in/out of the alternate screen as a side
	// effect of the bytes it contains (e.g. vim's own alt-screen
	// enter/exit escape sequences arriving in that chunk), independent
	// of whatever IsAltScreen() reported before that particular Write.
	altScreenAfterWrite *bool
	// cursorX/cursorY back CursorPosition — test-controlled, since the
	// fake doesn't track real cursor movement the way the vt library does.
	cursorX, cursorY int
}

func (f *fakeEmulator) Write(p []byte) (int, error) {
	f.written = append(f.written, p...)
	if len(f.pushOnWrite) > 0 {
		f.sbLines = append(f.sbLines, f.pushOnWrite...)
		f.pushOnWrite = nil
	}
	if f.altScreenAfterWrite != nil {
		f.altScreen = *f.altScreenAfterWrite
		f.altScreenAfterWrite = nil
	}
	return len(p), nil
}

func (f *fakeEmulator) Render() string {
	return string(f.written)
}

func (f *fakeEmulator) Resize(w, h int) {
	f.w, f.h = w, h
	f.resizes++
}

func (f *fakeEmulator) ScrollbackLen() int {
	return len(f.sbLines)
}

func (f *fakeEmulator) CursorPosition() (x, y int) {
	return f.cursorX, f.cursorY
}

func (f *fakeEmulator) ScrollbackLine(index int) string {
	if index < 0 || index >= len(f.sbLines) {
		return ""
	}
	return f.sbLines[index]
}

func (f *fakeEmulator) IsAltScreen() bool {
	return f.altScreen
}

func withFakes(t *testing.T, p *fakePty, e *fakeEmulator) {
	t.Helper()
	origPty, origEmu := newPty, newEmulator
	newPty = func(width, height int) (Pty, error) { return p, nil }
	newEmulator = func(width, height int) Emulator { return e }
	t.Cleanup(func() {
		newPty = origPty
		newEmulator = origEmu
	})
}

func TestStartSpawnsOnlyOnce(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1)
	spawns := 0
	origPty := newPty
	newPty = func(width, height int) (Pty, error) {
		spawns++
		return p, nil
	}
	t.Cleanup(func() { newPty = origPty })

	m, _ = m.Start()
	m, _ = m.Start()

	if spawns != 1 {
		t.Fatalf("got %d spawns, want 1", spawns)
	}
	if !m.Started() {
		t.Fatal("expected Started() to be true")
	}
}

func TestStartSurfacesPtyCreationError(t *testing.T) {
	origPty := newPty
	newPty = func(width, height int) (Pty, error) {
		return nil, errors.New("boom")
	}
	t.Cleanup(func() { newPty = origPty })

	m := New(1)
	m, _ = m.Start()

	if got := m.View(); got != "Terminal error: terminal: boom" {
		t.Fatalf("got %q", got)
	}
}

func TestStartSurfacesCommandStartError(t *testing.T) {
	p := &fakePty{startErr: errors.New("no shell")}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1)
	m, _ = m.Start()

	if got := m.View(); got != "Terminal error: terminal: no shell" {
		t.Fatalf("got %q", got)
	}
}

func TestOutputMsgWritesIntoEmulatorAndReschedulesRead(t *testing.T) {
	p := &fakePty{toRead: [][]byte{[]byte("more")}}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1)
	m, cmd := m.Start()
	if cmd == nil {
		t.Fatal("expected a read command from Start")
	}

	msg := cmd()
	outMsg, ok := msg.(OutputMsg)
	if !ok {
		t.Fatalf("got %T, want OutputMsg", msg)
	}

	m, cmd = m.Update(outMsg)
	if string(e.written) != string(outMsg.data) {
		t.Fatalf("emulator got %q, want %q", e.written, outMsg.data)
	}
	if cmd == nil {
		t.Fatal("expected Update to reschedule another read")
	}
}

func TestStaleOutputMsgFromOldGenerationIsIgnored(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1)
	m, _ = m.Start()

	stale := OutputMsg{data: []byte("old"), generation: m.generation - 1}
	m, cmd := m.Update(stale)

	if len(e.written) != 0 {
		t.Fatalf("expected stale message to be ignored, got %q", e.written)
	}
	if cmd != nil {
		t.Fatal("expected no command for a stale message")
	}
}

func TestKeyPressWritesEncodedBytesToPty(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1)
	m, _ = m.Start()

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ls")})

	if string(p.writes) != "ls" {
		t.Fatalf("got %q, want %q", p.writes, "ls")
	}
}

func TestKeyPressBeforeStartIsANoOp(t *testing.T) {
	m := New(1)
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd != nil {
		t.Fatal("expected no command when the terminal was never started")
	}
	_ = m
}

func TestSetSizeOnlyResizesWhenSizeActuallyChanges(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1)
	m, _ = m.Start()
	m = m.SetSize(80, 24)
	m = m.SetSize(80, 24)
	m = m.SetSize(80, 24)

	if p.resizes != 1 {
		t.Fatalf("got %d pty resizes, want 1 (idempotent for unchanged size)", p.resizes)
	}
	if e.resizes != 1 {
		t.Fatalf("got %d emulator resizes, want 1", e.resizes)
	}

	m = m.SetSize(100, 30)
	if p.resizes != 2 {
		t.Fatalf("got %d pty resizes after a real size change, want 2", p.resizes)
	}
}

func TestCloseClosesThePty(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1)
	m, _ = m.Start()

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !p.closed {
		t.Fatal("expected the pty to be closed")
	}
}

func TestCloseBeforeStartIsANoOp(t *testing.T) {
	m := New(1)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestViewShowsTerminalNotStartedBeforeFirstFocus(t *testing.T) {
	m := New(1)
	if got := m.View(); got != "Terminal not started" {
		t.Fatalf("got %q", got)
	}
}

// Regression test: an aggressive window resize can shrink the app's
// computed pane width below zero before it reaches SetSize (see
// app.Model's WindowSizeMsg handling) — a negative width/height reaching
// the real vt.Emulator's Resize panics with "slice bounds out of range"
// deep inside its buffer resize (not something this package can catch
// after the fact). SetSize must clamp to zero itself rather than trust
// the caller, since a third-party library's Resize has no obligation to
// handle negative input gracefully.
func TestSetSizeClampsNegativeDimensionsToZero(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1)
	m, _ = m.Start()
	m = m.SetSize(-5, 23)

	if e.w < 0 || e.h < 0 {
		t.Fatalf("got emulator resize (%d, %d), want both clamped to >= 0", e.w, e.h)
	}
	if p.resizeW < 0 || p.resizeH < 0 {
		t.Fatalf("got pty resize (%d, %d), want both clamped to >= 0", p.resizeW, p.resizeH)
	}
}

func TestIDReturnsWhatNewWasConstructedWith(t *testing.T) {
	m := New(7)
	if got := m.ID(); got != 7 {
		t.Fatalf("got ID()=%d, want 7", got)
	}
}

func TestOutputMsgAndReadErrMsgEchoTheSessionID(t *testing.T) {
	p := &fakePty{toRead: [][]byte{[]byte("hi")}}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(42)
	m, cmd := m.Start()
	if cmd == nil {
		t.Fatal("test setup: Start() returned a nil cmd")
	}
	msg := cmd()
	out, ok := msg.(OutputMsg)
	if !ok {
		t.Fatalf("test setup: got %T, want OutputMsg", msg)
	}
	if got := out.ID(); got != 42 {
		t.Fatalf("got OutputMsg.ID()=%d, want 42", got)
	}

	// Drain the pty dry so the next read reports an error, producing a
	// ReadErrMsg to check the same way.
	m, cmd = m.Update(msg)
	if cmd == nil {
		t.Fatal("test setup: Update(OutputMsg) returned a nil cmd")
	}
	msg = cmd()
	errMsg, ok := msg.(ReadErrMsg)
	if !ok {
		t.Fatalf("test setup: got %T, want ReadErrMsg", msg)
	}
	if got := errMsg.ID(); got != 42 {
		t.Fatalf("got ReadErrMsg.ID()=%d, want 42", got)
	}
}

func TestTwoSessionsIDsNeverCollideEvenThoughBothGenerationCountersStartAtZero(t *testing.T) {
	p1 := &fakePty{toRead: [][]byte{[]byte("a")}}
	e1 := &fakeEmulator{}
	p2 := &fakePty{toRead: [][]byte{[]byte("b")}}
	e2 := &fakeEmulator{}

	origPty, origEmu := newPty, newEmulator
	t.Cleanup(func() { newPty, newEmulator = origPty, origEmu })

	newPty = func(width, height int) (Pty, error) { return p1, nil }
	newEmulator = func(width, height int) Emulator { return e1 }
	m1 := New(1)
	m1, cmd1 := m1.Start()

	newPty = func(width, height int) (Pty, error) { return p2, nil }
	newEmulator = func(width, height int) Emulator { return e2 }
	m2 := New(2)
	m2, cmd2 := m2.Start()

	out1 := cmd1().(OutputMsg)
	out2 := cmd2().(OutputMsg)
	if out1.ID() == out2.ID() {
		t.Fatalf("got both sessions' OutputMsg.ID()=%d, want distinct ids (1 vs 2) even though both instances' internal generation counters started at the same value", out1.ID())
	}
	if out1.ID() != 1 || out2.ID() != 2 {
		t.Fatalf("got ids (%d, %d), want (1, 2)", out1.ID(), out2.ID())
	}
}

func TestScrollLinesClampsToZeroAndToScrollbackLength(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"a", "b", "c"}}
	withFakes(t, p, e)

	m := New(1).SetSize(10, 3)
	m, _ = m.Start()

	m = m.ScrollLines(1) // positive n scrolls down; already at the bottom
	if m.scrollOffset != 0 {
		t.Fatalf("got scrollOffset=%d after scrolling down from the bottom, want 0 (can't scroll past following live output)", m.scrollOffset)
	}

	m = m.ScrollLines(-100) // scroll way up, past all available scrollback
	if m.scrollOffset != 3 {
		t.Fatalf("got scrollOffset=%d after scrolling far past available scrollback, want 3 (clamped to ScrollbackLen())", m.scrollOffset)
	}
}

func TestViewAtZeroOffsetIsUnchangedFromPriorBehavior(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"old0", "old1"}}
	withFakes(t, p, e)

	m := New(1).SetSize(10, 3)
	m, _ = m.Start()
	e.written = []byte("live0\nlive1\nlive2")

	if got, want := m.View(), "live0\nlive1\nlive2"; got != want {
		t.Fatalf("got View()=%q at scrollOffset=0, want %q — exactly Render()'s own output, unchanged from before scrollback existed (no scrollbar overlay while following)", got, want)
	}
}

func TestViewWhenScrolledUpShowsScrollbackContentAndDropsOldestLiveLine(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"old0", "old1", "old2", "old3", "old4"}}
	withFakes(t, p, e)

	m := New(1).SetSize(20, 3)
	m, _ = m.Start()
	e.written = []byte("live0\nlive1\nlive2")

	m = m.ScrollLines(-1) // scroll up by 1 line

	view := m.View()
	if !strings.Contains(view, "old4") {
		t.Fatalf("expected the scrolled view to show old4 (the newest scrollback line), got %q", view)
	}
	if !strings.Contains(view, "live0") || !strings.Contains(view, "live1") {
		t.Fatalf("expected the scrolled view to still show live0 and live1, got %q", view)
	}
	if strings.Contains(view, "live2") {
		t.Fatalf("expected the scrolled view to have dropped live2 (the newest live line) off the bottom, got %q", view)
	}
	if got := strings.Count(view, "\n"); got != 2 {
		t.Fatalf("got %d newlines in the scrolled view, want 2 (3 rows)", got)
	}
}

func TestKeyPressResumesFollowingAfterScrollingUp(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"old0", "old1"}}
	withFakes(t, p, e)

	m := New(1).SetSize(10, 3)
	m, _ = m.Start()
	m = m.ScrollLines(-1)
	if m.scrollOffset == 0 {
		t.Fatal("test setup: expected scrollOffset > 0 after scrolling up")
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = updated

	if m.scrollOffset != 0 {
		t.Fatalf("got scrollOffset=%d after a keypress, want 0 (any input resumes following live output)", m.scrollOffset)
	}
}

func TestOutputReceivedWhilePausedKeepsViewedWindowPinned(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"old0", "old1", "old2"}, pushOnWrite: []string{"old3", "old4"}}
	withFakes(t, p, e)

	m := New(1).SetSize(10, 3)
	m, _ = m.Start()
	m = m.ScrollLines(-1)
	if m.scrollOffset != 1 {
		t.Fatalf("test setup: got scrollOffset=%d, want 1", m.scrollOffset)
	}

	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("more")})
	m = updated

	if m.scrollOffset != 3 {
		t.Fatalf("got scrollOffset=%d after 2 new scrollback lines arrived while paused, want 3 (1 + 2 — the viewed window stays pinned to the same absolute content as new output pushes the live bottom down)", m.scrollOffset)
	}
}

func TestOutputReceivedWhileFollowingStaysAtOffsetZero(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{pushOnWrite: []string{"old0", "old1"}}
	withFakes(t, p, e)

	m := New(1).SetSize(10, 3)
	m, _ = m.Start()

	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("more")})
	m = updated

	if m.scrollOffset != 0 {
		t.Fatalf("got scrollOffset=%d after output arrived while following, want 0 (nothing to pin — the live view keeps following automatically)", m.scrollOffset)
	}
}

// TestScrollLinesIsANoOpDuringAltScreen and
// TestViewIgnoresStaleScrollOffsetWhenAltScreenBecomesActive cover a real
// correctness bug caught by review on the original version of this
// feature: vt.Emulator.Scrollback() always reports the MAIN screen's
// scrollback, even while a full-screen app (vim, htop, less) has switched
// to the alternate screen — so scrolling during one of those apps used to
// splice stale pre-app shell history in among the app's own live rows.

func TestScrollLinesIsANoOpDuringAltScreen(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"old0", "old1", "old2"}, altScreen: true}
	withFakes(t, p, e)

	m := New(1).SetSize(10, 3)
	m, _ = m.Start()

	m = m.ScrollLines(-1)

	if m.scrollOffset != 0 {
		t.Fatalf("got scrollOffset=%d after scrolling during alt screen, want 0 (real terminals don't scroll into stale main-screen history while a full-screen app owns the display)", m.scrollOffset)
	}
}

func TestViewIgnoresStaleScrollOffsetWhenAltScreenBecomesActive(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"old0", "old1", "old2"}}
	withFakes(t, p, e)

	m := New(1).SetSize(10, 3)
	m, _ = m.Start()
	m = m.ScrollLines(-1) // paused, viewing scrollback, still on the main screen
	if m.scrollOffset == 0 {
		t.Fatal("test setup: expected scrollOffset > 0 after scrolling up")
	}

	// Simulate a full-screen app (vim, htop, less) taking over the
	// display without the user scrolling again in between — matches how
	// the real vt.Emulator's IsAltScreen() flips the moment the app's
	// escape sequence arrives, independent of anything this package does.
	e.altScreen = true
	e.written = []byte("vim-line-A\nvim-line-B\nvim-line-C")

	view := m.View()
	want := "vim-line-A\nvim-line-B\nvim-line-C"
	if view != want {
		t.Fatalf("got View()=%q while alt screen is active with a stale scrollOffset, want %q — the live alt-screen content, not blended with stale main-screen scrollback", view, want)
	}
}

// TestRenderScrolledViewPadsShortLinesSoTheScrollbarStaysAtAFixedColumn
// covers a legitimate finding from PR #8's Greptile bot review:
// lipgloss.NewStyle().MaxWidth() only truncates lines longer than the
// target width, it never pads shorter ones — so a scrollbar appended
// directly after each (unpadded) line landed at a different column on
// every row, drifting left/right depending on each row's own content
// length, instead of staying fixed at the pane's right edge.
func TestRenderScrolledViewPadsShortLinesSoTheScrollbarStaysAtAFixedColumn(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"a", "bb", "ccc"}}
	withFakes(t, p, e)

	m := New(1).SetSize(20, 3)
	m, _ = m.Start()
	e.written = []byte("live0\nlive1\nlive2")
	m = m.ScrollLines(-1)

	lines := strings.Split(m.View(), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	col0 := strings.IndexAny(lines[0], "│█")
	if col0 < 0 {
		t.Fatalf("test setup: row 0 has no scrollbar character at all: %q", lines[0])
	}
	for i, line := range lines {
		col := strings.IndexAny(line, "│█")
		if col != col0 {
			t.Fatalf("got scrollbar column=%d on row %d (%q), want %d (same as row 0) — the scrollbar must stay at a fixed column regardless of each row's own content length", col, i, line, col0)
		}
	}
}

// TestScrollOffsetResetsWhenAltScreenExitsDuringAnOutputMsg covers a real
// gap in the earlier alt-screen fix, caught by CodeRabbit's review of PR
// #8: View() correctly falls back to the plain live render WHILE the alt
// screen is active, but scrollOffset itself was never reset — so once an
// alt-screen app (vim, less, ...) exits, IsAltScreen() goes back to false
// and View() would immediately re-enter renderScrolledView() with the
// STALE pre-app scrollOffset, blending old main-screen scrollback into
// the just-returned live prompt for a frame (until the next scroll or
// keypress happened to reset it).
func TestScrollOffsetResetsWhenAltScreenExitsDuringAnOutputMsg(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"old0", "old1", "old2"}}
	withFakes(t, p, e)

	m := New(1).SetSize(10, 3)
	m, _ = m.Start()
	m = m.ScrollLines(-1) // paused, viewing scrollback, on the main screen
	if m.scrollOffset == 0 {
		t.Fatal("test setup: expected scrollOffset > 0 after scrolling up")
	}

	// Simulate: an alt-screen app started at some point after that (the
	// already-covered transition), and is now exiting within this single
	// OutputMsg — IsAltScreen() reports true going in, false coming out.
	e.altScreen = true
	exiting := false
	e.altScreenAfterWrite = &exiting
	e.written = []byte("prompt-returned")

	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("more")})
	m = updated

	if m.scrollOffset != 0 {
		t.Fatalf("got scrollOffset=%d after an alt-screen app exited mid-OutputMsg, want 0 (a stale offset from before the app started must not blend into the just-returned live prompt)", m.scrollOffset)
	}
}

// TestRenderScrolledViewAtZeroWidthRendersNoGutter and
// TestRenderScrolledViewAtWidthOneShowsOnlyTheScrollbarCell cover another
// CodeRabbit finding on PR #8: at m.width == 0, renderScrolledView's own
// `if overlayWidth > 0` guard correctly skipped the (now-negative-width)
// content truncation, but still unconditionally appended the 2-cell
// " "+bar gutter afterward — so a degenerately narrow pane (the same
// aggressively-downsized-window class this codebase already floors
// elsewhere) rendered rows wider than its own claimed width instead of
// nothing at all.

func TestRenderScrolledViewAtZeroWidthRendersNoGutter(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"a", "b", "c"}}
	withFakes(t, p, e)

	m := New(1).SetSize(0, 3)
	m, _ = m.Start()
	e.written = []byte("live0\nlive1\nlive2")
	m = m.ScrollLines(-1)

	for i, line := range strings.Split(m.View(), "\n") {
		if got := lipgloss.Width(line); got != 0 {
			t.Fatalf("got row %d width=%d at pane width 0, want 0 (no content, no gutter): %q", i, got, line)
		}
	}
}

func TestRenderScrolledViewAtWidthOneShowsOnlyTheScrollbarCell(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"a", "b", "c"}}
	withFakes(t, p, e)

	m := New(1).SetSize(1, 3)
	m, _ = m.Start()
	e.written = []byte("live0\nlive1\nlive2")
	m = m.ScrollLines(-1)

	for i, line := range strings.Split(m.View(), "\n") {
		if got := lipgloss.Width(line); got != 1 {
			t.Fatalf("got row %d width=%d at pane width 1, want 1 (scrollbar cell only, no content, no leading space): %q", i, got, line)
		}
	}
}

// TestRenderScrolledViewAtWidthTwoDoesNotExceedPaneWidth covers a gap the
// zero- and one-width guards above didn't close, caught by review: at
// exactly width 2, overlayWidth (m.width-2) is 0, which fell through to the
// default branch. Lip Gloss's MaxWidth skips truncation entirely at 0
// (rather than collapsing to an empty string), so a non-empty line passed
// through untruncated, and the gutter/scrollbar appended after it pushed
// the row past m.width.
func TestRenderScrolledViewAtWidthTwoDoesNotExceedPaneWidth(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"a much longer line than the pane is wide", "b", "c"}}
	withFakes(t, p, e)

	m := New(1).SetSize(2, 3)
	m, _ = m.Start()
	e.written = []byte("live0\nlive1\nlive2")
	m = m.ScrollLines(-1)

	for i, line := range strings.Split(m.View(), "\n") {
		if got := lipgloss.Width(line); got != 2 {
			t.Fatalf("got row %d width=%d at pane width 2, want 2: %q", i, got, line)
		}
	}
}

// TestSetSizeCapturesRowsAHeightShrinkWouldOtherwiseDiscard and
// TestSetSizePreservesRowsLostToARealEmulatorsHeightShrink cover a real,
// user-reported bug: cat a big file, then shrink the terminal pane (drag
// the boundary, or resize the whole window), and the live content —
// including the shell prompt — visibly vanished instead of scrolling into
// history.
//
// Root cause, confirmed directly in the vendored library source
// (charmbracelet/x/ultraviolet's Buffer.Resize): a height shrink is
// implemented as `b.Lines = b.Lines[:height]` — keeping the grid's TOP
// rows and discarding everything below, with no scrollback push at all.
// Since the cursor (and therefore the most recently written content, like
// a shell prompt) sits near the BOTTOM of the grid, this is exactly
// backwards.
//
// SetSize's unified resize computation corrects it by re-deriving the
// whole grid itself and writing the result back afterward
// (writeReflowedRows), which overwrites whatever the library's own
// resize did — so the library's "keep the top" behavior no longer
// constrains anything. The direction is the one a real terminal uses:
// the NEWEST rows (including the cursor's, e.g. the shell prompt) stay
// live, and the OLDEST rows that no longer fit are evicted oldest-first
// into m.shrinkOverflow — a tier between the real scrollback and the
// live screen that renderScrolledView already knows how to render.

func TestSetSizeCapturesRowsAHeightShrinkWouldOtherwiseDiscard(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1).SetSize(20, 5)
	m, _ = m.Start()
	e.written = []byte("line0\nline1\nline2\nline3\nline4")

	m = m.SetSize(20, 2) // shrink from 5 rows to 2

	// The unified resize computation always evicts the OLDEST rows and
	// keeps the NEWEST live, independent of whatever the library's own
	// resize does to cell content — writeReflowedRows overwrites the
	// grid afterward regardless, so the library's "keep Lines[:2]"
	// behavior no longer constrains which end we evict.
	//
	// The NEW direction keeps the newest rows (line3, line4 — nearest
	// the cursor) live, evicting the OLDEST (line0-line2) to
	// shrinkOverflow — the corrected behavior: a real terminal scrolls
	// old content into history and keeps your current prompt visible,
	// not the other way around.
	want := []string{"line0", "line1", "line2"}
	if len(m.shrinkOverflow) != len(want) {
		t.Fatalf("got shrinkOverflow=%q, want %q (the oldest rows, evicted to make room for the newest ones to stay live)", m.shrinkOverflow, want)
	}
	for i, w := range want {
		if m.shrinkOverflow[i] != w {
			t.Fatalf("got shrinkOverflow[%d]=%q, want %q", i, m.shrinkOverflow[i], w)
		}
	}
	if got := e.Render(); !strings.Contains(got, "line3") || !strings.Contains(got, "line4") {
		t.Fatalf("got live Render()=%q, want it to show line3 and line4 (the newest rows, kept live)", got)
	}
}

// TestSetSizeTrimsTrailingBlankPaddingFromShrinkCapture covers a gap found
// by review: the emulator's grid is always a full m.height rows, so
// anything below the cursor is blank padding, not real content. Capturing
// liveLines[height:] on a shrink can include that padding when the cursor
// sits above the new cutoff — storing it would render as spurious blank
// rows in shrinkOverflow and, over a repeated shrink/grow bounce, spend the
// maxShrinkOverflow budget on nothing but blank lines.
func TestSetSizeTrimsTrailingBlankPaddingFromShrinkCapture(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1).SetSize(20, 5)
	m, _ = m.Start()
	// The cursor is on row 2 ("line2"); rows 3 and 4 are unwritten blank
	// padding, same as the real emulator would report them.
	e.written = []byte(strings.Join([]string{"line0", "line1", "line2", "", ""}, "\n"))

	m = m.SetSize(20, 2) // shrink from 5 rows to 2: discards rows 2-4

	// oldRows trims down to ["line0","line1","line2"] (cursor is on
	// row2; rows 3-4 are blank padding). Shrinking 3 rows to height 2
	// evicts the oldest 1 ("line0"), keeping the newest 2 ("line1",
	// "line2") live.
	want := []string{"line0"}
	if len(m.shrinkOverflow) != len(want) || m.shrinkOverflow[0] != want[0] {
		t.Fatalf("got shrinkOverflow=%q, want %q (blank padding trimmed before eviction, and only the oldest meaningful row evicted)", m.shrinkOverflow, want)
	}
}

func TestSetSizePreservesRowsLostToARealEmulatorsHeightShrink(t *testing.T) {
	// Uses the REAL vt.Emulator (only newPty is faked, to avoid spawning
	// a real shell) — a fake emulator doesn't reproduce the destructive
	// resize behavior this test exists to guard against, so verifying
	// against the fake wouldn't prove anything about the actual bug.
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(20, 5)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	// Write 5 lines directly through Update — no read loop needed, this
	// just exercises the same m.emu.Write path a real OutputMsg would.
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("line0\r\nline1\r\nline2\r\nline3\r\nline4")})
	m = updated

	if !strings.Contains(m.View(), "line4") {
		t.Fatalf("test setup: expected the live view to show line4 before shrinking; got %q", m.View())
	}

	m = m.SetSize(20, 2) // shrink from 5 rows to 2

	// The NEW direction keeps the newest rows (line3, line4 — nearest
	// the cursor, e.g. a shell prompt) live with no scrolling needed at
	// all — this is the actual fix: the prompt never disappears.
	if view := m.View(); !strings.Contains(view, "line3") || !strings.Contains(view, "line4") {
		t.Fatalf("got View()=%q immediately after shrinking, want it to show line3 and line4 live — the newest rows must stay visible without any scrolling", view)
	}

	// line2 (the newest of the EVICTED rows) must be reachable by
	// scrolling up just slightly — it's the row immediately behind the
	// live view.
	oneUp := m.ScrollLines(-1)
	if view := oneUp.View(); !strings.Contains(view, "line2") {
		t.Fatalf("got View()=%q after scrolling up 1 line post-shrink, want it to contain line2 (the newest evicted row, immediately behind the live view)", view)
	}
	// line0 (the OLDEST evicted row) must be reachable by scrolling all
	// the way up, proving the full evicted range survived.
	allUp := m.ScrollLines(-10)
	if view := allUp.View(); !strings.Contains(view, "line0") {
		t.Fatalf("got View()=%q after scrolling all the way up post-shrink, want it to contain line0 (the oldest evicted row)", view)
	}
}

func TestSetSizeShrinkCaptureIsSkippedDuringAltScreen(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{altScreen: true}
	withFakes(t, p, e)

	m := New(1).SetSize(20, 5)
	m, _ = m.Start()
	e.written = []byte("vim-a\nvim-b\nvim-c\nvim-d\nvim-e")

	m = m.SetSize(20, 2) // shrink while an alt-screen app is showing

	if len(m.shrinkOverflow) != 0 {
		t.Fatalf("got shrinkOverflow=%q after shrinking during the alt screen, want empty — capturing the alt-screen app's own UI here would surface as unrelated content blended into main-screen scrollback later", m.shrinkOverflow)
	}
}

func TestSetSizeShrinkCaptureIsCappedLikeTheRealScrollbackBuffer(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1).SetSize(20, 2)
	m, _ = m.Start()

	// Repeatedly grow to 3 rows (writing a fresh, distinguishable row
	// each time) then shrink back to 2, so every cycle captures exactly
	// one more row — enough cycles to push shrinkOverflow's length past
	// maxShrinkOverflow if nothing caps it.
	const cycles = maxShrinkOverflow + 5
	for i := 0; i < cycles; i++ {
		m = m.SetSize(20, 3)
		e.written = []byte("a\nb\nrow")
		m = m.SetSize(20, 2)
	}

	if len(m.shrinkOverflow) != maxShrinkOverflow {
		t.Fatalf("got len(shrinkOverflow)=%d after %d shrink cycles, want it capped at maxShrinkOverflow=%d (mirroring the real emulator's own scrollback cap)", len(m.shrinkOverflow), cycles, maxShrinkOverflow)
	}
}

func TestSetSizeDoesNotCaptureOnAWidthOnlyShrink(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1).SetSize(20, 5)
	m, _ = m.Start()
	e.written = []byte("line0\nline1\nline2\nline3\nline4")

	m = m.SetSize(10, 5) // width shrinks, height unchanged

	if len(m.shrinkOverflow) != 0 {
		t.Fatalf("got shrinkOverflow=%q after a width-only shrink, want empty — a width-only shrink of content that doesn't need extra rows to reflow leaves shrinkOverflow untouched (see TestSetSizeReflow* for the cases that do overflow)", m.shrinkOverflow)
	}
}

// TestSequentialSmallShrinksProduceTheSameOrderAsOneBigShrink covers a
// real, user-reported bug found through manual testing in Ghostty (a real
// terminal emulator): dragging a real window's edge delivers MANY small,
// separate resize events as the OS reports each intermediate size — not
// one big jump the way tmux's `resize-window` (used to verify this
// feature originally) does. An earlier, gesture-tracking version of this
// code ordered each step's captured rows relative to the previous step's,
// which came out backwards after a multi-step shrink even though a
// single-step shrink of the same total size was correct. SetSize's
// unified computation is stateless — it re-derives the whole grid from
// scratch on every call and always evicts the oldest rows — so a
// multi-step drag and one big jump covering the same range must now
// produce identical shrinkOverflow contents, which is what this checks.
//
// Uses the REAL vt.Emulator (only newPty is faked) for both the
// multi-step and single-jump sides of the comparison — the fake's Write
// doesn't reproduce the library's actual resize/discard behavior, so
// comparing against it wouldn't prove anything about the real bug.
func TestSequentialSmallShrinksProduceTheSameOrderAsOneBigShrink(t *testing.T) {
	newSession := func(t *testing.T) Model {
		t.Helper()
		p := &fakePty{}
		origPty := newPty
		newPty = func(width, height int) (Pty, error) { return p, nil }
		t.Cleanup(func() { newPty = origPty })

		m := New(1).SetSize(20, 8)
		m, _ = m.Start()
		t.Cleanup(func() { _ = m.Close() })
		updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("r0\r\nr1\r\nr2\r\nr3\r\nr4\r\nr5\r\nr6\r\nr7")})
		return updated
	}

	oneJump := newSession(t)
	oneJump = oneJump.SetSize(20, 1)

	stepwise := newSession(t)
	for h := 7; h >= 1; h-- {
		stepwise = stepwise.SetSize(20, h)
	}

	if len(oneJump.shrinkOverflow) != len(stepwise.shrinkOverflow) {
		t.Fatalf("got %d entries from the stepwise shrink, %d from the one-jump shrink covering the same total range — want equal", len(stepwise.shrinkOverflow), len(oneJump.shrinkOverflow))
	}
	for i := range oneJump.shrinkOverflow {
		if oneJump.shrinkOverflow[i] != stepwise.shrinkOverflow[i] {
			t.Fatalf("got stepwise shrinkOverflow=%q, want it to match the one-jump shrink's order %q (a real terminal window drag arrives as many small steps, not one jump — both must produce the same oldest-first order)", stepwise.shrinkOverflow, oneJump.shrinkOverflow)
		}
	}
}

// TestShrinkOverflowRendersBetweenRealScrollbackAndLiveNotBeforeScrollback
// covers a second real bug found alongside the one above: shrinkOverflow
// was rendered as a tier OLDER than the emulator's own real scrollback,
// but the rows it holds were on the LIVE screen at capture time — newer
// than anything already scrolled into real scrollback by then (the
// common case: heavy output naturally fills real scrollback first, then
// a shrink captures whatever's left on the live screen at that moment).
// With the bug, scrolling up from the live view had to pass through the
// ENTIRE real scrollback before reaching the content a shrink had just
// captured — for a big scrollback, that made the just-lost content
// effectively unreachable by any normal "scroll up a bit" gesture,
// exactly matching the reported symptom ("only the last ~20 lines are
// cropped").
func TestShrinkOverflowRendersBetweenRealScrollbackAndLiveNotBeforeScrollback(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{sbLines: []string{"old0", "old1", "old2"}}
	withFakes(t, p, e)

	m := New(1).SetSize(20, 5)
	m, _ = m.Start()
	e.written = []byte("mid0\nmid1\nmid2\nmid3\nmid4")

	m = m.SetSize(20, 2) // evicts mid0, mid1, mid2 (oldest) into shrinkOverflow; mid3, mid4 (newest) stay live

	// Scrolling up by 1 from the live view (2 visible rows) should reach
	// straight into shrinkOverflow's newest entry (mid2) — NOT into the
	// real scrollback's "old*" entries, which are older and must require
	// scrolling further to reach.
	nearby := m.ScrollLines(-1)
	if view := nearby.View(); !strings.Contains(view, "mid2") {
		t.Fatalf("got View()=%q after scrolling up 1 line, want it to contain mid2 (shrinkOverflow's newest entry, immediately behind the live view — not buried behind the real scrollback)", view)
	}
	if view := nearby.View(); strings.Contains(view, "old2") {
		t.Fatalf("got View()=%q after scrolling up only 1 line, want it to NOT yet reach old2 (the real scrollback's newest entry, which is older than anything shrinkOverflow holds and should require scrolling further)", view)
	}

	// Scrolling all the way up must eventually reach the real scrollback
	// too — it isn't discarded, just correctly positioned as older.
	allTheWayUp := m.ScrollLines(-100)
	if view := allTheWayUp.View(); !strings.Contains(view, "old0") {
		t.Fatalf("got View()=%q after scrolling all the way up, want it to contain old0 (the real scrollback's oldest entry, still reachable)", view)
	}
}

// TestSetSizeReflowsWidthShrinkWithoutLoss covers the bug this plan
// exists to fix, verified against the REAL vt.Emulator (the fake
// doesn't reproduce the library's destructive resize behavior — see
// this package's other real-emulator tests): shrinking the pane's
// width must not lose any characters — they reflow into more, narrower
// rows instead of being truncated.
func TestSetSizeReflowsWidthShrinkWithoutLoss(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(40, 5)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	const line = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123" // 31 chars, fits at width 40 with no wrap
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(line)})
	m = updated

	m = m.SetSize(10, 5) // shrink width 40 -> 10: must reflow into 4 rows, not truncate to "ABCDEFGHIJ"

	view := m.View()
	for _, want := range []string{"ABCDEFGHIJ", "KLMNOPQRST", "UVWXYZ0123"} {
		if !strings.Contains(view, want) {
			t.Fatalf("got View()=%q after shrinking, want it to contain %q (reflowed, not truncated)", view, want)
		}
	}
}

// TestSetSizeReflowsWidthGrowRejoinsWrappedContent is the other
// direction: widening back out must rejoin what a prior shrink wrapped,
// not leave it stuck wrapped at the old narrow width.
func TestSetSizeReflowsWidthGrowRejoinsWrappedContent(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(10, 5)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	const line = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123"
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(line)})
	m = updated // wraps across 4 rows at width 10

	m = m.SetSize(40, 5) // grow back — must rejoin into one row

	if !strings.Contains(m.View(), line) {
		t.Fatalf("got View()=%q after growing back to the original width, want the full rejoined line %q", m.View(), line)
	}
}

// TestSetSizeReflowLeavesNoStaleTailWhenARowGetsShorter asserts the
// FULL grid content (not just that the reflowed text appears somewhere
// in it): reflow's write-back recomputes whole rows, so any row whose
// new content is SHORTER than what that row held before must have its
// old tail erased, not left showing past the end of the new content.
// Growing 10 -> 12 with "1234567890abcde" on the grid rewraps to
// ["1234567890ab", "cde"]; writing "cde" over the old "abcde" without
// erasing first leaves "cdede" — the old "de" surviving as garbage.
// Uses the REAL vt.Emulator (only newPty is faked), since the fake's
// Render() just echoes bytes written and can't reproduce a grid
// overwrite at all.
func TestSetSizeReflowLeavesNoStaleTailWhenARowGetsShorter(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(10, 5)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("1234567890abcde")})
	m = updated // grid rows: "1234567890" / "abcde"

	m = m.SetSize(12, 5) // grow width only: rewraps to "1234567890ab" / "cde"

	got := strings.Split(m.View(), "\n")
	want := []string{"1234567890ab", "cde", "", "", ""}
	if len(got) != len(want) {
		t.Fatalf("got %d rows %q, want %d rows %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got rows %q, want %q — row %d differs (a stale tail from the pre-reflow content was left behind)", got, want, i)
		}
	}
}

// TestSetSizeReflowShrinkThenGrowLeavesNoDuplicateRows is the same
// erase requirement one row lower: rows the reflow no longer needs at
// all (because the content rejoined into fewer, wider rows) must be
// blanked, not left holding the previous, narrower wrapping. The design
// spec's §5 says freed rows are left blank — without an explicit erase
// they instead keep showing stale duplicates of content that now also
// appears, correctly rejoined, above them.
func TestSetSizeReflowShrinkThenGrowLeavesNoDuplicateRows(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(40, 10)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	const line = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" // 36 chars: one row at width 40, four at width 10
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(line)})
	m = updated

	m = m.SetSize(10, 10) // shrink: wraps across 4 rows
	m = m.SetSize(40, 10) // grow back: rejoins into 1 row, freeing rows 2-4

	got := strings.Split(m.View(), "\n")
	if len(got) != 10 {
		t.Fatalf("got %d rows %q, want 10 (the pane's height)", len(got), got)
	}
	if got[0] != line {
		t.Fatalf("got first row %q, want the fully rejoined line %q", got[0], line)
	}
	for i, row := range got[1:] {
		if strings.TrimSpace(row) != "" {
			t.Fatalf("got row %d = %q, want it blank — the narrower wrapping from before the grow was left behind as a stale duplicate (full grid: %q)", i+1, row, got)
		}
	}
}

// TestSetSizeReflowsStyledContentWithoutHangingOrLoss runs COLORIZED
// content — the normal case for any real shell, and what every other
// test in this feature happened to miss — through the whole pipeline
// against the REAL vt.Emulator. Pre-fix this hung the entire app on the
// very first width change (see
// TestRewrapLogicalLineWithStyledContentDoesNotHang for the mechanism),
// so it runs under a timeout: a regression must fail the test rather
// than wedge the suite. The assertions are on the FULL grid shape, so a
// stale tail or duplicated row would be caught too, and on the styling
// still being present after each step.
func TestSetSizeReflowsStyledContentWithoutHangingOrLoss(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(10, 5)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	// Red background across a line long enough to wrap at width 10.
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("\x1b[41m1234567890abcde\x1b[0m")})
	m = updated

	type step struct {
		width int
		want  []string
	}
	steps := []step{
		{12, []string{"\x1b[41m1234567890ab\x1b[m", "\x1b[41mcde\x1b[m", "", "", ""}},
		{6, []string{"\x1b[41m123456\x1b[m", "\x1b[41m7890ab\x1b[m", "\x1b[41mcde\x1b[m", "", ""}},
		{40, []string{"\x1b[41m1234567890abcde\x1b[m", "", "", "", ""}},
	}
	for _, s := range steps {
		done := make(chan Model, 1)
		go func(m Model, width int) { done <- m.SetSize(width, 5) }(m, s.width)
		select {
		case m = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("SetSize(%d, 5) hung on ANSI-styled grid content — reflow's rewrap loop is spinning on a zero-width styled remainder", s.width)
		}
		got := strings.Split(m.View(), "\n")
		if len(got) != len(s.want) {
			t.Fatalf("at width %d: got %d rows %q, want %d rows %q", s.width, len(got), got, len(s.want), s.want)
		}
		for i := range s.want {
			if got[i] != s.want[i] {
				t.Fatalf("at width %d: got rows %q, want %q — row %d differs", s.width, got, s.want, i)
			}
		}
	}
}

// TestSetSizeReflowRespondsToOutputArrivingMidResize is the core
// property that makes this approach robust where PR #8's snapshot
// attempt wasn't (see the design spec's Overview): reflow recomputes
// fresh from whatever the live grid currently holds on every call, so
// output arriving between two resize steps is naturally reflected
// correctly on the next one — there is no stale snapshot to compare
// against or invalidate.
func TestSetSizeReflowRespondsToOutputArrivingMidResize(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(40, 5)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("original text here")})
	m = updated

	m = m.SetSize(10, 5) // shrink

	// New output arrives before the pane grows back — simulating a
	// shell redrawing its prompt in response to this exact resize.
	updated, _ = m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("\rreplaced")})
	m = updated

	m = m.SetSize(40, 5) // grow back

	if strings.Contains(m.View(), "original text") {
		t.Fatalf("got View()=%q, want the genuinely newer content, not stale pre-output text restored over it", m.View())
	}
	if !strings.Contains(m.View(), "replaced") {
		t.Fatalf("got View()=%q, want the newer output present", m.View())
	}
}

// TestSetSizeReflowOverflowAppendsIntoShrinkOverflow covers the height
// interaction from the design spec's §5: a width-only reflow that needs
// MORE rows than fit in the unchanged height pushes the excess (oldest,
// from the top) into shrinkOverflow rather than discarding it.
// TestSetSizeReflowDoesNotReflowContentAlreadyInShrinkOverflow pins a
// documented, accepted limitation (design spec §8): once reflow
// overflow lands in shrinkOverflow at whatever width it was pushed
// there, a LATER, separate width-changing resize only reflows the
// current live grid — it never revisits content already sitting in
// shrinkOverflow. This means shrinkOverflow can end up holding rows
// wrapped at different historical widths after multiple resizes, with
// no attempt to reconcile them. Not a bug to fix here — shrinkOverflow
// is exactly as historical/frozen as real scrollback already is, and
// re-wrapping it on every subsequent resize would mean re-wrapping
// potentially thousands of historical rows each time, the same cost
// already declined for real scrollback.
func TestSetSizeReflowDoesNotReflowContentAlreadyInShrinkOverflow(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(40, 2)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	const line = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(line)})
	m = updated

	m = m.SetSize(10, 2) // first width-only shrink: pushes 2 rows into shrinkOverflow, wrapped at width 10
	if len(m.shrinkOverflow) == 0 {
		t.Fatal("test setup: expected the first shrink to produce reflow overflow")
	}
	frozenAtWidth10 := append([]string(nil), m.shrinkOverflow...)

	m = m.SetSize(5, 2) // second, separate width-only shrink

	if len(m.shrinkOverflow) < len(frozenAtWidth10) {
		t.Fatalf("got %d shrinkOverflow entries after the second shrink, want at least the %d already there to remain untouched", len(m.shrinkOverflow), len(frozenAtWidth10))
	}
	for i, want := range frozenAtWidth10 {
		if m.shrinkOverflow[i] != want {
			t.Fatalf("got shrinkOverflow[%d]=%q after a second, separate width-only resize, want it unchanged at %q — content already in shrinkOverflow is never re-reflowed by a later resize (see design spec §8)", i, m.shrinkOverflow[i], want)
		}
	}
}

func TestSetSizeReflowOverflowAppendsIntoShrinkOverflow(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(40, 2) // only 2 rows tall
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	// One line that fits in a single row at width 40, but needs 4 rows
	// once rewrapped at width 10 — more than the height (2) can hold.
	const line = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(line)})
	m = updated

	m = m.SetSize(10, 2) // shrink width only; height stays 2

	if len(m.shrinkOverflow) == 0 {
		t.Fatalf("got empty shrinkOverflow, want the rows that didn't fit in height 2 to have been pushed there")
	}
	// The live view (scrolled all the way up through shrinkOverflow)
	// must still contain the full original content — nothing lost.
	allUp := m.ScrollLines(-10)
	if !strings.Contains(allUp.View(), "ABCDEFGH") {
		t.Fatalf("got View()=%q after scrolling up through shrinkOverflow, want the earliest reflowed row present", allUp.View())
	}
}

// TestSetSizeReflowOverflowPinsAPausedViewport covers a viewport-drift
// bug: when a width-only resize's reflow overflow grows shrinkOverflow
// while the user is paused (scrollOffset > 0), the combined buffer
// (scrollback + shrinkOverflow + live) that renderScrolledView measures
// against just got longer — without also growing scrollOffset by the
// same amount, the paused viewport silently shifts toward the live
// tail, exactly the class of bug Update's OutputMsg case already guards
// against for real output arriving while paused (see its own
// beforeLen/afterLen comment) — this is the same guard, for the same
// reason, on the reflow-overflow path.
func TestSetSizeReflowOverflowPinsAPausedViewport(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(40, 2) // only 2 rows tall
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	// L0 scrolls into real scrollback (1 line); the live grid ends up
	// showing L1 and the long line — the long line fits one row at
	// width 40 but needs 4 rows once rewrapped at width 10, more than
	// the height (2) can hold, so shrinking width overflows it.
	const longLine = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("L0\r\nL1\r\n" + longLine)})
	m = updated

	m = m.ScrollLines(-1) // pause, into the real scrollback
	if m.scrollOffset == 0 {
		t.Fatal("test setup: expected scrollOffset > 0 after scrolling up")
	}
	pausedOffset := m.scrollOffset
	overflowBefore := len(m.shrinkOverflow)

	m = m.SetSize(10, 2) // width-only shrink while paused

	overflowAdded := len(m.shrinkOverflow) - overflowBefore
	if overflowAdded == 0 {
		t.Fatal("test setup: expected the width shrink to actually produce reflow overflow")
	}
	if want := pausedOffset + overflowAdded; m.scrollOffset != want {
		t.Fatalf("got scrollOffset=%d after reflow overflow while paused, want %d (paused offset %d + %d newly-overflowed rows) — otherwise the paused viewport drifts toward the live tail", m.scrollOffset, want, pausedOffset, overflowAdded)
	}
}

// TestSetSizeReflowSkippedDuringAltScreen mirrors the existing
// shrinkOverflow alt-screen guard: reflow must not run while a
// full-screen app (vim, less, ...) is active, or it would corrupt that
// app's own UI by reflowing it as if it were shell history.
func TestSetSizeReflowSkippedDuringAltScreen(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{altScreen: true}
	withFakes(t, p, e)

	m := New(1).SetSize(40, 5)
	m, _ = m.Start()

	before := e.written
	m = m.SetSize(10, 5)

	if string(e.written) != string(before) {
		t.Fatalf("got e.written changed during an alt-screen resize, want reflow's write-back to have been skipped entirely")
	}
}

// TestSetSizeReflowsWidthOnASimultaneousWidthAndHeightChange is the
// actual bug this plan exists to fix: a real corner-drag changes width
// and height together on every intermediate step, and a prior version
// of this feature skipped reflow entirely whenever that happened,
// leaving the width dimension completely unprotected — reproduced by
// simulating exactly this shape of resize against the real vt.Emulator
// and confirming no truncation.
func TestSetSizeReflowsWidthOnASimultaneousWidthAndHeightChange(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(40, 10)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	const line = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123" // 31 chars, fits at width 40 with no wrap
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(line)})
	m = updated

	m = m.SetSize(10, 5) // BOTH width and height shrink in the same call

	view := m.View()
	for _, want := range []string{"ABCDEFGHIJ", "KLMNOPQRST", "UVWXYZ0123"} {
		if !strings.Contains(view, want) {
			// May need to scroll up to reach some of the reflowed rows,
			// since height also shrank to 5 — check the fully scrolled
			// view too before failing.
			allUp := m.ScrollLines(-10)
			if !strings.Contains(allUp.View(), want) {
				t.Fatalf("got View()=%q (live) and %q (scrolled up), want %q reachable somewhere — reflowed, not truncated, even though height changed in the same call", view, allUp.View(), want)
			}
		}
	}
}

// TestSetSizeSimulatedCornerDragPreservesContentBothWays is the closest
// unit-level approximation of the real corner-drag tmux reproduction:
// several sequential SetSize calls each changing BOTH dimensions,
// shrinking then growing back, mirroring how a real window corner-drag
// delivers many small simultaneous-dimension steps rather than one
// jump.
func TestSetSizeSimulatedCornerDragPreservesContentBothWays(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(150, 45)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	const line = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnopqrstuvwxyz"
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(line)})
	m = updated

	// Shrink corner-drag: both dimensions shrink together, several steps.
	for _, sz := range [][2]int{{130, 40}, {110, 36}, {90, 32}, {70, 28}, {60, 25}} {
		m = m.SetSize(sz[0], sz[1])
	}
	// Grow corner-drag: both dimensions grow back together, several steps.
	for _, sz := range [][2]int{{70, 28}, {90, 32}, {110, 36}, {130, 40}, {150, 45}} {
		m = m.SetSize(sz[0], sz[1])
	}

	// The full original line must be reachable somewhere — live or
	// scrolled — not truncated mid-word at any point in the round trip.
	found := strings.Contains(m.View(), line)
	if !found {
		allUp := m.ScrollLines(-50)
		found = strings.Contains(allUp.View(), line)
	}
	if !found {
		t.Fatalf("got line not found intact anywhere after a simulated corner-drag round trip (live view=%q), want it reachable and un-truncated", m.View())
	}
}

// TestSetSizeSequentialResizesDoNotDropAWordBoundarySpace is the
// regression test for the bug reported after the width+height
// unification fix shipped: a real corner-drag (many sequential SetSize
// calls, both dimensions changing every step, nothing typed in
// between) was still destroying content — a real space character
// between two words got silently and permanently dropped, and by the
// end of a longer drag, unrelated words were fused together with no
// space at all.
//
// Root cause: groupIntoLogicalLines decides "did row i wrap into row
// i+1" from the ROW'S RENDERED STRING (isRowFilledToEdge) — but the
// real vt.Emulator strips a row's trailing blank cell when rendering
// it back to a string, so a wrap boundary that lands exactly after a
// space (extremely common in prose) makes the row measure one column
// SHORT, and the heuristic wrongly concludes "not a continuation" —
// permanently losing that space, not just miscounting it. Because
// EVERY SetSize call re-derives this from scratch, that single mistake
// can seed a DIFFERENT, unrelated misjudgment on a later resize step,
// compounding across a long drag.
//
// This test deliberately engineers a wrap boundary landing exactly on
// a space, confirms that boundary really does trip the known
// rendered-string ambiguity (sanity-checking the repro itself, not
// just hoping it does), then drives 24 sequential SetSize calls with
// no output in between — mirroring the density of a real window-corner
// drag, denser than the 5-6-jump verification that let this bug ship
// undetected the first time — and asserts the space and every word
// survive completely intact at the end.
func TestSetSizeSequentialResizesDoNotDropAWordBoundarySpace(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(118, 35)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	text := "  from rendered rows rather than raw terminal cells. Content that scrolls\r\n" +
		"  off the pane's visible area (from a height shrink or from reflow\r\n" +
		"  running out of room) is also never re-wrapped again by a later resize\r\n" +
		"  it stays wrapped at whatever width it was at when it scrolled off,\r\n"
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(text)})
	m = updated

	// Sanity-check the repro's own premise before relying on it: at
	// width 46, the first logical line's first physical row really
	// does measure one column short of "filled to the edge" (46),
	// because the wrap boundary lands right after a space. This has to
	// be checked against the REAL vt.Emulator's own write+render round
	// trip (not rewrapLogicalLine's pure in-memory output, which never
	// strips anything) — it's specifically the real emulator's
	// trailing-blank-cell stripping on render that produces the
	// one-column-short measurement this whole bug depends on. If this
	// ever stops being true (e.g. the emulator's rendering changes),
	// this test would otherwise silently stop testing anything.
	oneLine := "  from rendered rows rather than raw terminal cells. Content that scrolls"
	sanityEmu := newEmulator(46, 10)
	sanityEmu.Write([]byte(oneLine))
	firstRowAt46 := strings.Split(sanityEmu.Render(), "\n")[0]
	if isRowFilledToEdge(firstRowAt46, 46) {
		t.Fatalf("repro premise broken: row %q unexpectedly measures filled-to-edge at width 46 — this test needs updating, it is no longer exercising the missed-join ambiguity", firstRowAt46)
	}

	// 46 steps, both dimensions changing on every one: 118x58 down to
	// 26x35 and back — denser than the 30-step floor CLAUDE.md sets for
	// resize verification, since compounding can surface late.
	var widths []struct{ w, h int }
	for w, h := 114, 57; w >= 26; w, h = w-4, h-1 {
		widths = append(widths, struct{ w, h int }{w, h})
	}
	for w, h := 30, 36; w <= 118; w, h = w+4, h+1 {
		widths = append(widths, struct{ w, h int }{w, h})
	}
	if len(widths) < 30 {
		t.Fatalf("test setup: got %d resize steps, want 30+", len(widths))
	}
	for _, wh := range widths {
		m = m.SetSize(wh.w, wh.h)
	}

	view := m.View()
	for _, want := range []string{
		"rather than raw terminal cells",
		"Content that scrolls",
		"height shrink or from reflow",
		"is also never re-wrapped again by a later resize",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("final view missing intact phrase %q (word boundary dropped or content fused) — full view:\n%s", want, view)
		}
	}
	if strings.Contains(view, "  ") {
		// Two consecutive spaces anywhere outside the deliberate
		// leading indent would be suspicious, but leading indents are
		// legitimate — check instead for the SPECIFIC glued-word
		// failure signatures this bug actually produced.
		for _, glued := range []string{"terminalcells", "ratherthan", "scrollsoff"} {
			if strings.Contains(view, glued) {
				t.Fatalf("got glued-together words containing %q in view:\n%s", glued, view)
			}
		}
	}
}

// TestSetSizeDenseResizeSurvivesShellPromptRedrawAfterEveryStep is the
// regression test for the continuation-tracking fix never engaging
// against a real interactive shell: every pty resize delivers SIGWINCH,
// and zsh (bash/readline likewise) answers each one by redrawing its
// prompt — "\r\r", a few SGR resets, "\x1b[J", then the prompt text
// (captured verbatim from a real tmux-driven drag). That redraw arrives
// as an OutputMsg between EVERY pair of SetSize calls in a real drag,
// and Update used to throw the whole tracked record away on any output
// at all — so every step fell back to the rendered-string heuristic and
// the missed-joins compounded exactly as if the tracking didn't exist
// (content left fragmented into short rows after a shrink-and-regrow).
// TestSetSizeSequentialResizesDoNotDropAWordBoundarySpace above couldn't
// catch this: it chains SetSize calls with no output in between, which
// a real shell never does.
//
// The redraw only rewrites the prompt row, so every row above it must
// keep its tracked state — and the original lines must come back as
// the same single rows they started as once the width is restored.
func TestSetSizeDenseResizeSurvivesShellPromptRedrawAfterEveryStep(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(118, 58)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	lines := []string{
		"  from rendered rows rather than raw terminal cells. Content that scrolls",
		"  off the pane's visible area (from a height shrink or from reflow",
		"  running out of room) is also never re-wrapped again by a later resize",
		"  it stays wrapped at whatever width it was at when it scrolled off,",
	}
	const prompt = "user@host cody % "
	const promptRedraw = "\r\r\x1b[0m\x1b[27m\x1b[24m\x1b[J" + prompt
	text := strings.Join(lines, "\r\n") + "\r\n" + prompt
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(text)})
	m = updated

	// 46 steps, both dimensions changing on every one: 118x58 down to
	// 26x35 and back — denser than the 30-step floor CLAUDE.md sets for
	// resize verification, since compounding can surface late.
	var widths []struct{ w, h int }
	for w, h := 114, 57; w >= 26; w, h = w-4, h-1 {
		widths = append(widths, struct{ w, h int }{w, h})
	}
	for w, h := 30, 36; w <= 118; w, h = w+4, h+1 {
		widths = append(widths, struct{ w, h int }{w, h})
	}
	if len(widths) < 30 {
		t.Fatalf("test setup: got %d resize steps, want 30+", len(widths))
	}
	for _, wh := range widths {
		m = m.SetSize(wh.w, wh.h)
		updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(promptRedraw)})
		m = updated
	}

	rows := strings.Split(m.View(), "\n")
	for i, want := range lines {
		if i >= len(rows) || strings.TrimRight(rows[i], " ") != want {
			t.Fatalf("row %d: got %q, want the original line %q rejoined intact — full view:\n%s", i, rows[i], want, m.View())
		}
	}
	if got := strings.TrimRight(rows[len(lines)], " "); got != strings.TrimRight(prompt, " ") {
		t.Fatalf("got prompt row %q, want %q — full view:\n%s", got, prompt, m.View())
	}
}

// TestUpdateOutputUntracksOnlyTheRowsItChanged pins the per-row
// invalidation rule: real output must stop the tracked wrap record
// being trusted for the wrap boundary under any row it actually
// rewrote — this package has no idea what the shell just did there —
// but every boundary under a row it left alone keeps its state. That
// includes the prompt row here staying a known NON-continuation of the
// full-width row above it while it's typed into: the heuristic alone
// would false-join the two. (Dropping the WHOLE record on any output, as this
// used to, meant a real shell's SIGWINCH prompt redraw wiped it between
// every pair of resizes in a drag — see
// TestSetSizeDenseResizeSurvivesShellPromptRedrawAfterEveryStep.)
func TestUpdateOutputUntracksOnlyTheRowsItChanged(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(40, 10)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	const text = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123\r\n$ "
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(text)})
	m = updated

	m = m.SetSize(10, 10) // rows: ABCDEFGHIJ / KLMNOPQRST / UVWXYZ0123 / "$ "
	for i, tracked := range m.rowTracked {
		if !tracked {
			t.Fatalf("got row %d untracked right after a reflow, want every row tracked", i)
		}
	}

	// Typed at the prompt: only row 3 changes.
	updated, _ = m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("ls")})
	m = updated
	if got, want := m.rowTracked[:5], []bool{true, true, true, true, false}; !slicesEqual(got, want) {
		t.Fatalf("got rowTracked[:5]=%v after output on row 3, want %v — a row's bit records whether the row ABOVE wrapped into it, so only row 4's (above it: the changed row 3) is lost", got, want)
	}
	if got, want := m.rowContinues[:3], []bool{false, true, true}; !slicesEqual(got, want) {
		t.Fatalf("got rowContinues[:3]=%v, want %v carried through unchanged", got, want)
	}

	m = m.SetSize(40, 10)
	rows := strings.Split(m.View(), "\n")
	if got := strings.TrimRight(rows[0], " "); got != "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123" {
		t.Fatalf("got row 0 %q after widening, want the wrapped line rejoined intact", got)
	}
	if got := strings.TrimRight(rows[1], " "); got != "$ ls" {
		t.Fatalf("got row 1 %q after widening, want the prompt row %q on its own", got, "$ ls")
	}
}

// TestUpdateOutputThatScrollsShiftsTheTrackedRecordWithTheContent:
// output that scrolls the screen moves every surviving row up, and its
// tracked state must move with it — matched against the row it came
// from, not the unrelated row that used to sit at the same index.
func TestUpdateOutputThatScrollsShiftsTheTrackedRecordWithTheContent(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(40, 4)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	// "two words " wraps at width 10 with the boundary landing right
	// after a space — the case only the tracked record gets right.
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("first\r\nsome words and more\r\n$ ")})
	m = updated
	m = m.SetSize(10, 4) // rows: first / "some words" / " and more" / "$ "... exact split asserted below
	before := strings.Split(m.View(), "\n")

	// Two newlines at the bottom row scroll the screen by two.
	updated, _ = m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("\r\n\r\n$ ")})
	m = updated
	after := strings.Split(m.View(), "\n")
	if after[0] != before[2] {
		t.Fatalf("test setup: got row 0 %q after scrolling, want what was row 2 (%q)", after[0], before[2])
	}
	if m.rowTracked == nil || !m.rowTracked[0] || !m.rowTracked[1] {
		t.Fatalf("got rowTracked=%v after a 2-line scroll, want rows 0-1 (formerly rows 2-3) still tracked", m.rowTracked)
	}
	if !m.rowContinues[0] {
		t.Fatalf("got rowContinues[0]=false, want the continuation bit of what was row 2 carried up with it")
	}
}

// TestUpdateScrollingOutputAtScrollbackCapDropsTheTrackedRecord: the
// scroll shift is read off the scrollback's growth, which stops being
// exact once the scrollback is full — each line pushed in evicts one
// from the far end, so growth under-reports (here: reads 0 for a real
// 2-line scroll). Rows compared at that wrong offset could match by
// coincidence (repeated content) and inherit an unrelated row's state,
// so a write that evicts must drop the record instead. A write that
// doesn't scroll — the SIGWINCH prompt redraw — must keep working at
// any scrollback size, or the whole fix would quietly switch itself
// off in any long-lived session.
func TestUpdateScrollingOutputAtScrollbackCapDropsTheTrackedRecord(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })
	origEmu := newEmulator
	newEmulator = func(width, height int) Emulator {
		e := vt.NewEmulator(width, height)
		e.Scrollback().SetMaxLines(3)
		return vtEmulator{e}
	}
	t.Cleanup(func() { newEmulator = origEmu })

	m := New(1).SetSize(40, 4)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	// Distinct lines fill the 3-line scrollback to its cap; the live
	// screen then holds REPEATED rows — the case where comparing at the
	// wrong offset matches anyway.
	fill := "u\r\nv\r\nw\r\nx\r\ny\r\nsame\r\nsame\r\nsame\r\n$ "
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(fill)})
	m = updated
	if got := m.emu.ScrollbackLen(); got != 3 {
		t.Fatalf("test setup: got scrollback len %d, want it at its cap of 3", got)
	}

	m = m.SetSize(30, 4)
	if m.rowTracked == nil {
		t.Fatalf("test setup: got no tracked record after a reflow")
	}

	updated, _ = m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("\r\r\x1b[J$ ")})
	m = updated
	if m.rowTracked == nil {
		t.Fatalf("got the tracked record dropped by a non-scrolling prompt redraw at the scrollback cap, want it kept")
	}

	// Turn the prompt row into another "same" and scroll by one: the
	// screen ends up rendering exactly as before.
	beforeView, beforeLen := m.View(), m.emu.ScrollbackLen()
	updated, _ = m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("\r\x1b[Ksame\r\n$ ")})
	m = updated
	if m.View() != beforeView || m.emu.ScrollbackLen() != beforeLen {
		t.Fatalf("test setup: want a 1-line scroll invisible to both the rendered rows and the scrollback length; got view %q -> %q, len %d -> %d", beforeView, m.View(), beforeLen, m.emu.ScrollbackLen())
	}
	if m.rowTracked != nil {
		t.Fatalf("got rowTracked=%v after output that scrolled at the scrollback cap, want the record dropped — every row matched at shift 0, but the real shift was 1 and is unknowable", m.rowTracked)
	}
}

// TestSetSizeCursorLandingExactlyAtTheNewRightEdgeMovesToAContinuationRow
// covers both ways a cursor ends up one past the pane's last column
// after a shrink: its line (with or without a stripped trailing blank
// before the cursor) now fills the new width exactly. The grid has no
// such column — positioning there clamps the cursor back ONTO the last
// cell, and the next character typed overwrites it (the prompt's
// separating space, or the line's own last character). It must land at
// column 0 of a continuation row instead, the way a real terminal's
// pending-wrap state resolves, and rejoin its line on a later widen.
func TestSetSizeCursorLandingExactlyAtTheNewRightEdgeMovesToAContinuationRow(t *testing.T) {
	for _, tc := range []struct {
		name, printed, wantWide string
	}{
		{"trailing blank lands in the last cell", "123456789 ", "123456789 ls"},
		{"content itself fills the row", "1234567890", "1234567890ls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePty{}
			origPty := newPty
			newPty = func(width, height int) (Pty, error) { return p, nil }
			t.Cleanup(func() { newPty = origPty })

			m := New(1).SetSize(40, 5)
			m, _ = m.Start()
			t.Cleanup(func() { _ = m.Close() })

			updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(tc.printed)})
			m = updated
			m = m.SetSize(10, 5)
			updated, _ = m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("ls")})
			m = updated

			rows := strings.Split(m.View(), "\n")
			if got, want := strings.TrimRight(rows[0], " "), strings.TrimRight(tc.printed, " "); got != want {
				t.Fatalf("got row 0 %q after typing, want %q untouched", got, want)
			}
			if got := strings.TrimRight(rows[1], " "); got != "ls" {
				t.Fatalf("got row 1 %q, want the typed text %q on the continuation row", got, "ls")
			}

			m = m.SetSize(40, 5)
			if got := strings.TrimRight(strings.Split(m.View(), "\n")[0], " "); got != tc.wantWide {
				t.Fatalf("got row 0 %q after widening, want %q rejoined", got, tc.wantWide)
			}
		})
	}
}

// TestSetSizeHeightOnlyResizeDoesNotRecordFreshWrapsAsCertainNonContinuations
// covers the pass-through (width unchanged) branch: with no tracked
// record yet, it used to hand writeReflowedRows a nil record, which
// stored "not a continuation" for every row AS CERTAIN — so a
// genuinely wrapped line on screen during a height-only step (dragging
// the pane's top edge, or a corner-drag step where only the height
// moved) would refuse to rejoin on the next width change. The
// heuristic's answer must be what gets recorded instead.
func TestSetSizeHeightOnlyResizeDoesNotRecordFreshWrapsAsCertainNonContinuations(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(10, 6)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	const line = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123" // wraps to 3 full rows at width 10
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(line)})
	m = updated

	m = m.SetSize(10, 5) // height only
	m = m.SetSize(40, 5)
	if got := strings.TrimRight(strings.Split(m.View(), "\n")[0], " "); got != line {
		t.Fatalf("got row 0 %q after widening, want the wrapped line rejoined as %q", got, line)
	}
}

func slicesEqual(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSetSizeRowContinuesSurvivesHeightEvictionWithoutPanicking drives
// a resize sequence dense enough to evict rows into shrinkOverflow
// WHILE rowContinues is populated, asserting no index-out-of-range
// panic and that the surviving live rows' content is still correct
// after the eviction shifts both newRows and rowContinues together.
func TestSetSizeRowContinuesSurvivesHeightEvictionWithoutPanicking(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(80, 20)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	var lines strings.Builder
	for i := 0; i < 15; i++ {
		fmt.Fprintf(&lines, "line number %d of the test content\r\n", i)
	}
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(lines.String())})
	m = updated

	// First resize builds a tracked record.
	m = m.SetSize(40, 20)
	// Second resize shrinks HEIGHT enough to force eviction into
	// shrinkOverflow while rowContinues is populated from the first.
	m = m.SetSize(30, 5)
	// Third resize changes width again, consuming whatever tracked
	// record survived the eviction above.
	m = m.SetSize(60, 5)

	view := m.View()
	if !strings.Contains(view, "line number 14") {
		t.Fatalf("got view missing the most recent content after eviction:\n%s", view)
	}
}

// TestSetSizeRowContinuesTrimsToHeightOnPassThroughShrink covers a gap
// found by review: writeReflowedRows built m.rowContinues by copying
// newContinues and then padding it UP to newHeight if it was shorter,
// but never trimmed it DOWN if it was already longer. That longer case
// is reachable through SetSize's pass-through branch (width unchanged,
// only height changed) — there, newContinues is simply carried over as
// m.rowContinues from the PREVIOUS (taller) height, while newRows comes
// from oldRows trimmed down to whatever the render's actual content
// requires, which can legitimately be shorter than the new height too
// (so the eviction logic, keyed off len(newRows) vs newHeight, never
// fires to shorten it either). Left unfixed, m.rowContinues could stay
// longer than m.height indefinitely, contradicting its own field
// comment ("length == m.height when trustworthy").
func TestSetSizeRowContinuesTrimsToHeightOnPassThroughShrink(t *testing.T) {
	p := &fakePty{}
	e := &fakeEmulator{}
	withFakes(t, p, e)

	m := New(1).SetSize(20, 5)
	m, _ = m.Start()

	// A width-changing resize builds a tracked rowContinues record sized
	// to the (still tall) height of 5.
	e.written = []byte(strings.Join([]string{"line0", "line1", "line2", "line3", "line4"}, "\n"))
	e.cursorX, e.cursorY = 0, 4
	m = m.SetSize(10, 5)
	if len(m.rowContinues) != 5 {
		t.Fatalf("test setup: got len(rowContinues)=%d after the width-changing resize, want 5", len(m.rowContinues))
	}

	// Now a PASS-THROUGH resize (width stays 10, only height shrinks to
	// 2) with render content short enough that neither the lastMeaningful
	// trim nor writeReflowedRows' own eviction-into-shrinkOverflow logic
	// ever shortens the carried-over rowContinues — the only thing that
	// can still bring it down to the new height is the trim this test
	// guards.
	e.written = []byte("hello")
	e.cursorX, e.cursorY = 5, 0
	m = m.SetSize(10, 2)

	if len(m.rowContinues) != m.height {
		t.Fatalf("got len(rowContinues)=%d after a pass-through height shrink, want %d (== m.height)", len(m.rowContinues), m.height)
	}
}

// TestSetSizeSequentialResizesDoNotInsertSpuriousSpaceInWideCharacterContent
// is the regression test for a Critical bug found in code review of this
// package's padding-restoration fix (see SetSize's own comment): the fix
// originally assumed every row m.rowContinues[i] marks as a continuation
// must have been written at exactly m.width columns, padding any
// shortfall back up to m.width on the theory that the real vt.Emulator
// always strips it as trailing blank whitespace. That assumption is false
// — rewrapLogicalLine can legitimately leave a continuation row ONE
// column short of the target width when a trailing wide (double-width)
// character cluster doesn't fit and gets carried whole to the next row
// instead of being split (see rewrapLogicalLine's own doc comment,
// already pinned by TestRewrapLogicalLineNeverSplitsAWideCluster). Padding
// that row back to m.width with a space inserts a character that was
// never actually there, corrupting otherwise-correct wide-character
// content.
//
// This drives CJK content (each character two display columns wide)
// through a sequential resize chain dense enough to repeatedly force a
// width that can't evenly divide 2-column characters — guaranteeing at
// least one wide-cluster-carried, one-column-short continuation row
// somewhere in the sequence — then asserts the content survives with NO
// spurious space inserted anywhere between the characters.
func TestSetSizeSequentialResizesDoNotInsertSpuriousSpaceInWideCharacterContent(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(20, 10)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	const text = "你好世界"
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(text)})
	m = updated

	widths := []int{18, 16, 14, 12, 10, 8, 6, 4, 3, 4, 6, 8, 10, 12, 14, 16, 18, 20}
	for _, w := range widths {
		m = m.SetSize(w, 10)
	}

	// Reconstruct the wrapped content as one continuous string, since a
	// legitimate wrap at these narrow widths still puts characters on
	// separate physical rows — that's fine; only a genuinely INSERTED
	// space character is the bug.
	joined := strings.ReplaceAll(m.View(), "\n", "")
	joined = strings.TrimRight(joined, " ")
	if !strings.Contains(joined, text) {
		t.Fatalf("got content not intact as %q after resize sequence — full view:\n%s", text, m.View())
	}
	for _, spurious := range []string{"你 好", "好 世", "世 界", "你好 ", " 世界"} {
		if strings.Contains(m.View(), spurious) {
			t.Fatalf("got spurious space signature %q in view:\n%s", spurious, m.View())
		}
	}
}
