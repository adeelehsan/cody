package terminal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/xpty"

	"cody/internal/scrollbar"
)

// maxShrinkOverflow caps how many rows Model.shrinkOverflow will ever hold
// — mirroring the real vt.Emulator's own 10,000-line scrollback cap (see
// vt.DefaultScrollbackSize), so a long session across many shrink events
// (e.g. several drags of the pane boundary) can't grow this unboundedly.
// Oldest entries are dropped first, same eviction order as the library's
// own scrollback.
const maxShrinkOverflow = 10000

// OutputMsg carries a chunk of bytes read from the pty. Exported so
// internal/app can pass it through to Update unconditionally, the same
// way it already does for editor.RehighlightMsg.
type OutputMsg struct {
	id         int
	data       []byte
	generation int
}

// ID returns the terminal.Model this message belongs to (see New's doc
// comment) — a caller holding several sessions must check this before
// routing the message into any one of them.
func (m OutputMsg) ID() int {
	return m.id
}

// ReadErrMsg signals the pty's read loop stopped (the shell exited, or
// the pty was closed). Exported for the same reason as OutputMsg.
type ReadErrMsg struct {
	id         int
	generation int
}

// ID returns the terminal.Model this message belongs to — see
// OutputMsg.ID's doc comment.
func (m ReadErrMsg) ID() int {
	return m.id
}

type Model struct {
	id            int // caller-assigned; stable for this Model's lifetime, echoed in OutputMsg/ReadErrMsg for multi-instance routing (see New's doc comment)
	width, height int
	pty           Pty
	emu           Emulator
	started       bool
	err           error
	generation    int
	// scrollOffset is how many lines back from the live tail the viewport
	// currently shows: 0 means "following" (View() renders exactly
	// Render()'s live screen, unchanged from before this field existed);
	// >0 means paused, viewing scrollback (see ScrollLines, View,
	// renderScrolledView). Unlike editor/filetree's own scrollOffset,
	// there is no cursor here for it to track — scrolling the terminal
	// never moves anything the shell itself is doing, only the viewport.
	scrollOffset int
	// shrinkOverflow holds rows evicted by SetSize's unified resize
	// computation because they no longer fit the new height — oldest
	// first, rendered as a tier BETWEEN m.emu's own scrollback and the
	// live screen (see renderScrolledView, ScrollLines): the rows it
	// holds were on the live screen at capture time, so they are newer
	// than anything already scrolled into the real scrollback by then,
	// even though the real scrollback usually holds far more lines
	// overall (everything that scrolled off before this resize ever
	// happened). Always an append — SetSize's computation is stateless,
	// re-deriving fresh from the current grid on every call, so there is
	// no "continuing the same gesture" concept that would ever call for
	// a prepend.
	shrinkOverflow []string
	// rowContinues holds, for each of the current live rows (same
	// indexing as the grid itself, length == m.height when
	// trustworthy), whether that row is a soft-wrap continuation of
	// the row above it — as determined with certainty by this
	// package's own last reflow, not guessed from rendered text. nil
	// means "no trustworthy record for the current grid" (nothing has
	// been reflowed yet, or real output arrived and invalidated it) —
	// every consumer must treat nil exactly like "fall back to
	// isRowFilledToEdge's heuristic," so this can never make behavior
	// worse than before this field existed, only better. See the
	// design spec at docs/superpowers/specs/2026-09-11-reflow-continuation-tracking-design.md.
	rowContinues []bool
}

// New creates a terminal session identified by id — a value the caller
// controls and can use to tell this session's OutputMsg/ReadErrMsg apart
// from any other terminal.Model instance's. A per-instance generation
// counter alone can't do this: it starts at 0 for every instance, so two
// independently-created sessions' messages can carry the same generation
// and be indistinguishable. id is otherwise opaque to this package — never
// interpreted, only stored and echoed back.
func New(id int) Model {
	return Model{id: id}
}

// ID returns the identity this session was constructed with.
func (m Model) ID() int {
	return m.id
}

// SetSize resizes the pane. Resizing the underlying pty/emulator is a real
// syscall/state change, so it only happens when the size actually changed
// — this makes it safe to call every render frame (matching the pattern
// already used for the editor/tree panes) without spamming the child
// process with spurious resize notifications on every keystroke.
//
// width/height are clamped to >= 0 here rather than trusted from the
// caller: an aggressive window resize can drive the app's computed pane
// width negative (see app.Model's WindowSizeMsg handling), and the real
// vt.Emulator's Resize panics on a negative slice bound instead of
// degrading gracefully — this package can't rely on every caller getting
// the arithmetic right upstream, the same way editor/filetree already
// floor their own content width before using it.
//
// Any resize that changes width, height, or both runs one unified
// computation, protecting BOTH dimensions in the same call — a
// corner-drag (both dimensions changing on every intermediate step,
// which is how dragging a window corner actually behaves, not a rare
// edge case) needs its width protected exactly as much as a single-edge
// drag does, and an earlier version of this function that skipped width
// protection whenever height also changed left that gap wide open
// (confirmed by reproducing real truncation through it).
//
// ultraviolet's Buffer.Resize implements a width shrink as
// `Lines[i] = Lines[i][:width]` for every row — permanently truncating
// anything past the new column count, with no reflow of its own (the
// library has no per-row soft-wrap tracking to reflow from — confirmed
// directly in its source, and empirically against real terminals like
// tmux and Ghostty that DO reflow and don't lose this content) — and a
// height shrink as `Lines = Lines[:height]`, keeping the TOP rows and
// discarding the rest with no capture of its own. Both are corrected the
// same way: capture the old grid's rows and cursor before the library's
// own resize call, reflow them at the new width if width changed
// (reflowRows in reflow.go — a no-op pass-through if width didn't
// change), and write the result back afterward (writeReflowedRows)
// — which already erases every row it touches, so whatever the
// library's own resize did to cell content along the way is fully
// overwritten regardless of which dimension(s) changed.
//
// writeReflowedRows evicts whatever doesn't fit the new height into
// shrinkOverflow, oldest first, always keeping the NEWEST rows
// (including whichever holds the cursor) live — see its own doc
// comment. This one direction now covers a pure width change, a pure
// height change, and both together, replacing what used to be two
// separate mechanisms (a height-only capture with its own
// gesture-tracking flag, and a width-only reflow) that each only
// protected one dimension and, in the height-only path's case,
// evicted the wrong end of the grid — a workaround for the library's
// own resize always keeping the top, which no longer constrains
// anything once writeReflowedRows unconditionally overwrites the grid
// afterward regardless.
//
// This runs unconditionally on every resize call, recomputing fresh
// from the CURRENT grid every time — there is no snapshot to go stale,
// no "is this still the same gesture" tracking needed, which is what
// makes a real multi-step drag (many small SetSize calls, one per
// intermediate size the OS reports) and a single big jump covering the
// same total resize naturally produce identical results.
//
// Skipped entirely while the alt screen (vim, less, ...) is active:
// Render() would show that app's own UI, not shell history, and
// capturing or reflowing it here would later surface as unrelated
// content blended into what's supposed to be main-screen scrollback.
func (m Model) SetSize(width, height int) Model {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	changed := width != m.width || height != m.height
	doResize := changed && m.emu != nil && m.width > 0 && m.height > 0 && !m.emu.IsAltScreen()
	var newRows []string
	var newCursorRow, newCursorCol int
	var newContinues []bool
	if doResize {
		oldRows := strings.Split(m.emu.Render(), "\n")
		cx, cy := m.emu.CursorPosition()
		// The grid is always full-height, so any rows below the last
		// line of real content are blank padding, not something a real
		// terminal ever actually printed. Trimming down to whichever is
		// larger — the cursor's own row (it may itself sit on a blank
		// line) or the last non-blank row — keeps what follows from
		// treating that padding as content that needs a reserved slot.
		lastMeaningful := cy
		for i, row := range oldRows {
			if i > lastMeaningful && strings.TrimSpace(row) != "" {
				lastMeaningful = i
			}
		}
		if lastMeaningful+1 < len(oldRows) {
			oldRows = oldRows[:lastMeaningful+1]
		}
		// The real vt.Emulator strips trailing blank cells when it
		// renders a row back to a string — indistinguishable, cell by
		// cell, from a row that just never reached the edge (see
		// reflow.go's design notes). Whenever m.rowContinues already
		// knows FOR CERTAIN that row i continues row i-1, row i-1 MUST
		// have been exactly m.width columns wide the moment this
		// package wrote it — so any shortfall here is exactly that
		// stripped whitespace, not missing content, and restoring it
		// before reflow/pass-through is what keeps a wrap boundary that
		// lands on a space from silently losing that space on every
		// resize after the one that first produced it.
		for i := 1; i < len(oldRows) && i < len(m.rowContinues); i++ {
			if !m.rowContinues[i] {
				continue
			}
			if pad := m.width - ansi.StringWidth(oldRows[i-1]); pad > 0 {
				oldRows[i-1] += strings.Repeat(" ", pad)
			}
		}
		if width != m.width {
			newRows, newCursorRow, newCursorCol, newContinues = reflowRows(oldRows, m.width, width, cy, cx, m.rowContinues)
		} else {
			// Width didn't change — nothing to rewrap. The old rows,
			// cursor position, and known continuation bits all pass
			// through unchanged; writeReflowedRows below still handles
			// a height change on its own (evicting excess into
			// shrinkOverflow if the new height is smaller).
			newRows, newCursorRow, newCursorCol = oldRows, cy, cx
			newContinues = m.rowContinues
		}
	}
	m.width, m.height = width, height
	if changed {
		if m.pty != nil {
			m.pty.Resize(width, height)
		}
		if m.emu != nil {
			m.emu.Resize(width, height)
		}
	}
	if doResize {
		m = m.writeReflowedRows(newRows, newCursorRow, newCursorCol, height, newContinues)
	}
	return m
}

// writeReflowedRows writes newRows (already computed at the new width
// by reflowRows) into the live grid — which by the time this runs has
// already been resized to its final width/height — and repositions the
// cursor to (newCursorRow, newCursorCol), matching reflowRows' own
// (row, col) return order. Every row it touches is erased before being
// written, and every row from len(newRows) through newHeight-1 is
// erased outright: reflow recomputes the grid's whole content, so
// anything already there is stale by construction and writing over it
// without erasing leaves the old, longer content showing through (see
// the row loops' own comments). Rows beyond newHeight are appended into
// m.shrinkOverflow (see its own doc comment) rather than discarded,
// mirroring how a real terminal pushes reflow overflow into scrollback:
// this is always an append, never a prepend, since reflow has no
// "continuing the same gesture" concept (see reflowRows' own doc
// comment) — every call's overflow is, by construction, a fresh batch
// relative to whatever shrinkOverflow already holds.
//
// newContinues carries the known-continuation bits for newRows (see
// reflowRows' own doc comment) — shifted/trimmed here in lockstep with
// newRows whenever excess rows get evicted into shrinkOverflow, then
// stored as the new m.rowContinues (padded with false for any row from
// len(newRows) through newHeight-1, since blank padding is never a
// continuation of anything). This is what lets the NEXT SetSize call
// trust this resize's own output instead of re-deriving it from
// rendered strings — see the design spec at
// docs/superpowers/specs/2026-09-11-reflow-continuation-tracking-design.md.
func (m Model) writeReflowedRows(newRows []string, newCursorRow, newCursorCol, newHeight int, newContinues []bool) Model {
	if excess := len(newRows) - newHeight; excess > 0 {
		beforeOverflowLen := len(m.shrinkOverflow)
		m.shrinkOverflow = append(m.shrinkOverflow, newRows[:excess]...)
		if over := len(m.shrinkOverflow) - maxShrinkOverflow; over > 0 {
			m.shrinkOverflow = m.shrinkOverflow[over:]
		}
		// Pin a paused viewport the same way Update's OutputMsg case
		// already does when real output grows the combined buffer
		// underneath it: the combined buffer (scrollback + shrinkOverflow
		// + live) just grew by however much of this batch actually stuck
		// (after the maxShrinkOverflow trim above may have dropped some
		// of it from the front), so scrollOffset must grow by the same
		// amount, or renderScrolledView's paused viewport silently drifts
		// toward the live tail even though the user never asked it to.
		if m.scrollOffset > 0 {
			m.scrollOffset += len(m.shrinkOverflow) - beforeOverflowLen
		}
		newRows = newRows[excess:]
		if excess <= len(newContinues) {
			newContinues = newContinues[excess:]
		} else {
			newContinues = nil
		}
		newCursorRow -= excess
	}
	if newCursorRow < 0 {
		newCursorRow = 0
	}
	if newHeight > 0 && newCursorRow >= newHeight {
		newCursorRow = newHeight - 1
	}
	var buf strings.Builder
	for i, row := range newRows {
		if i >= newHeight {
			break
		}
		// \x1b[K (erase to end of line) before the content: reflow
		// recomputes the WHOLE content of every row it touches, so
		// anything already sitting in this row is stale by
		// construction. Writing without erasing leaves whatever the
		// row held before showing past the end of the new, shorter
		// content — e.g. rewrapping "1234567890"/"abcde" from width 10
		// to 12 writes "cde" over the old "abcde" and leaves "cdede".
		fmt.Fprintf(&buf, "\x1b[%d;1H\x1b[K%s", i+1, row)
	}
	// Rows the reflow doesn't reach at all still hold the PREVIOUS
	// wrapping and must be blanked too, or content that just rejoined
	// into fewer, wider rows above stays visible below as a stale
	// duplicate of itself (the design spec's §5 states freed rows are
	// left blank — this is what makes that true). Safe to erase
	// unconditionally: SetSize already trimmed reflowRows' input down to
	// the last meaningful row, so everything past newRows was blank
	// padding anyway.
	for i := len(newRows); i < newHeight; i++ {
		fmt.Fprintf(&buf, "\x1b[%d;1H\x1b[K", i+1)
	}
	if newHeight > 0 {
		fmt.Fprintf(&buf, "\x1b[%d;%dH", newCursorRow+1, newCursorCol+1)
	}
	m.rowContinues = append([]bool(nil), newContinues...)
	for len(m.rowContinues) < newHeight {
		m.rowContinues = append(m.rowContinues, false)
	}
	m.emu.Write([]byte(buf.String()))
	return m
}

func (m Model) Started() bool {
	return m.started
}

// ScrollLines shifts the terminal's viewport by n lines: negative scrolls
// up into scrollback, positive scrolls down toward the live tail. Clamped
// to [0, current scrollback length] — 0 always means "following the live
// output" (see View), and the top end tracks whatever scrollback the
// underlying emulator currently holds, so this never over- or
// under-scrolls even as the buffer grows or gets capped. A no-op before
// the session has started (m.emu == nil, nothing to measure a scrollback
// length against), and while the alt screen is active (see View's own
// alt-screen note) — there's nothing valid to scroll into.
func (m Model) ScrollLines(n int) Model {
	if m.emu == nil {
		return m
	}
	if m.emu.IsAltScreen() {
		m.scrollOffset = 0
		return m
	}
	m.scrollOffset -= n
	if maxOffset := m.emu.ScrollbackLen() + len(m.shrinkOverflow); m.scrollOffset > maxOffset {
		m.scrollOffset = maxOffset
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
	return m
}

// Start spawns $SHELL in a pseudo-terminal, if not already started, and
// returns a command that kicks off the continuous pty-read loop. Safe to
// call more than once — a no-op after the first call.
func (m Model) Start() (Model, tea.Cmd) {
	if m.started {
		return m, nil
	}
	m.started = true

	w, h := m.width, m.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}

	p, err := newPty(w, h)
	if err != nil {
		m.err = fmt.Errorf("terminal: %w", err)
		return m, nil
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	setControllingTerminal(cmd)
	if err := p.Start(cmd); err != nil {
		m.err = fmt.Errorf("terminal: %w", err)
		return m, nil
	}
	// Reap the child once it exits so it doesn't linger as a zombie for
	// the remaining life of the app. Fire-and-forget: the model doesn't
	// need to know when this completes.
	go func() { _ = xpty.WaitProcess(context.Background(), cmd) }()

	m.pty = p
	m.emu = newEmulator(w, h)
	m.generation++
	return m, readCmd(m.pty, m.id, m.generation)
}

func readCmd(p Pty, id, generation int) tea.Cmd {
	return func() tea.Msg {
		buf := make([]byte, 4096)
		n, err := p.Read(buf)
		if err != nil {
			return ReadErrMsg{id: id, generation: generation}
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		return OutputMsg{id: id, data: data, generation: generation}
	}
}

func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case OutputMsg:
		if msg.generation != m.generation || m.emu == nil {
			return m, nil
		}
		// If paused (viewing scrollback), keep the viewed window pinned to
		// the same absolute content as new output pushes the live bottom
		// down — matching real terminals, which don't yank a paused view
		// back toward the bottom just because more output arrived in the
		// background. beforeLen/afterLen bracket exactly the growth this
		// one Write caused; scrollOffset (lines back from the bottom)
		// grows by the same amount to compensate. A no-op while following
		// (scrollOffset == 0 stays 0 — nothing to pin).
		wasAltScreen := m.emu.IsAltScreen()
		var beforeLen int
		if m.scrollOffset > 0 {
			beforeLen = m.emu.ScrollbackLen()
		}
		m.emu.Write(msg.data)
		// Any real output can change rows this package previously
		// tracked as reflow continuations in ways it has no visibility
		// into (new content, a redraw, a prompt repaint) — the tracked
		// record can only be trusted for a resize-only sequence with
		// nothing typed in between, so it must not survive past this
		// point. The next SetSize call falls back to the heuristic for
		// this content (identical to today's behavior) and rebuilds a
		// fresh, trustworthy record from there for any FURTHER resize
		// in the same drag.
		m.rowContinues = nil
		switch {
		case wasAltScreen && !m.emu.IsAltScreen():
			// The alt screen (vim, less, ...) just exited as part of this
			// very Write. Any scrollOffset left over refers to
			// main-screen content from before it started — meaningless
			// now that we're back to the live prompt it just returned
			// to, and View's own alt-screen guard no longer masks it
			// (IsAltScreen() is false again) — reset before it can blend
			// stale scrollback into that prompt.
			m.scrollOffset = 0
		case m.scrollOffset > 0:
			m.scrollOffset += m.emu.ScrollbackLen() - beforeLen
		}
		return m, readCmd(m.pty, m.id, m.generation)
	case ReadErrMsg:
		// The shell exited (or the pty closed). Stop reading; the last
		// rendered frame stays visible. No restart in this phase (see
		// plan's Global Constraints).
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	if m.pty == nil {
		return m, nil
	}
	seq := encodeKey(msg)
	if seq == nil {
		return m, nil
	}
	// Any keystroke resumes following the live output — matches real
	// terminals: there's no point staying paused on scrollback once
	// you've started typing at the (live) shell again.
	m.scrollOffset = 0
	m.pty.Write(seq)
	return m, nil
}

func (m Model) View() string {
	if m.err != nil {
		return fmt.Sprintf("Terminal error: %s", m.err)
	}
	if m.emu == nil {
		return "Terminal not started"
	}
	// The alt screen (vim, htop, less, ...) doesn't share the main
	// screen's scrollback (vt.Emulator.Scrollback() always reports the
	// main screen's, regardless of which is active) — rendering scrolled
	// while it's up would splice unrelated, stale pre-app content in
	// among the app's own live rows. Fall back to the plain live render
	// unconditionally in that case, even if scrollOffset is still
	// sitting >0 from before the app started (ScrollLines resets it back
	// to 0 on the next scroll attempt, but View() can't wait for that —
	// it must render correctly on the very next frame regardless).
	if m.scrollOffset == 0 || m.emu.IsAltScreen() {
		return m.emu.Render()
	}
	return m.renderScrolledView()
}

// scrollbarStyle matches internal/editor and internal/filetree's own
// locally-defined style exactly (same color, same "defined per-package"
// convention) — kept local rather than shared so this package doesn't pick
// up a dependency on either of theirs for a two-line style value.
var scrollbarStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

// renderScrolledView renders the viewport m.scrollOffset lines back from
// the live tail, with a scrollbar overlaid on the last column of each row
// (internal/scrollbar's own Column, the same one editor/filetree already
// use). Only ever called when scrollOffset > 0 — the far more common
// scrollOffset == 0 case renders via Render() directly, completely
// unaffected by any of this (see View), so normal terminal use never pays
// for or sees this overlay.
//
// The vt library's Render() only ever renders the live screen — there is
// no "render at an offset" parameter — so a paused/scrolled view is built
// by hand from three chronologically-ordered tiers, oldest first: the
// emulator's own scrollback (ScrollbackLen/ScrollbackLine — normally far
// more content than a single shrink ever captures, everything that
// scrolled off before the shrink happened), then m.shrinkOverflow (rows a
// resize would otherwise have destroyed — either a height shrink, see
// SetSize, or a width-only reflow needing more rows than the current
// height holds, see writeReflowedRows — which were still on the live
// screen at capture time, making them newer than whatever was already in
// scrollback then), then the live screen's own rows. The live screen's
// individual rows come from splitting Render()'s own output on "\n" —
// safe because ANSI SGR/CSI escape sequences never contain a raw
// newline byte.
func (m Model) renderScrolledView() string {
	liveLines := strings.Split(m.emu.Render(), "\n")
	overflowLen := len(m.shrinkOverflow)
	sbLen := m.emu.ScrollbackLen()
	height := m.height
	if height <= 0 || height > len(liveLines) {
		height = len(liveLines)
	}
	total := sbLen + overflowLen + height
	start := max(0, total-height-m.scrollOffset)
	bar := scrollbar.Column(total, height, start)
	overlayWidth := m.width - 2 // " " + one scrollbar rune, matching editor/filetree's own scrollbarGutterWidth
	lines := make([]string, height)
	for row := range height {
		i := start + row
		var line string
		switch {
		case i < sbLen:
			// Real scrollback comes first (oldest): it holds everything
			// that scrolled off before this shrink ever happened, which
			// is normally far more content than a single shrink captures.
			line = m.emu.ScrollbackLine(i)
		case i-sbLen < overflowLen:
			// shrinkOverflow sits between scrollback and the live screen
			// — its rows were still on the live screen at capture time,
			// so they're newer than anything already in scrollback then
			// (see shrinkOverflow's own doc comment for the one edge
			// case this doesn't perfectly handle: real output arriving
			// AFTER a shrink and being pushed into scrollback afterward).
			line = m.shrinkOverflow[i-sbLen]
		case i-sbLen-overflowLen < len(liveLines):
			line = liveLines[i-sbLen-overflowLen]
		}
		var barRune rune
		if row < len(bar) {
			barRune = bar[row]
		}
		// A degenerately narrow pane (the same aggressively-downsized-
		// window class this codebase already floors elsewhere) must
		// never render a row wider than m.width itself claims: no room
		// for content or a gutter at 0, room for only the scrollbar cell
		// itself at 1 (no leading space), and the normal content+gutter
		// shape from 2 up.
		switch {
		case m.width <= 0:
			lines[row] = ""
		case m.width == 1:
			lines[row] = scrollbarStyle.Render(string(barRune))
		case overlayWidth <= 0:
			// m.width == 2: overlayWidth (m.width-2) is 0, and Lip Gloss's
			// MaxWidth skips truncation entirely at 0 rather than
			// collapsing the line to empty — falling through to the
			// default branch below would let a non-empty line pass
			// through untruncated and push the row past m.width.
			lines[row] = " " + scrollbarStyle.Render(string(barRune))
		default:
			// MaxWidth truncates a line longer than overlayWidth, but
			// (unlike editor/filetree's own padRow, which this mirrors)
			// never pads a shorter one — without padding, the scrollbar
			// appended right after would land at a different column on
			// every row, drifting with each row's own content length
			// instead of staying fixed at the pane's right edge.
			line = lipgloss.NewStyle().MaxWidth(overlayWidth).Render(line)
			if pad := overlayWidth - lipgloss.Width(line); pad > 0 {
				line += strings.Repeat(" ", pad)
			}
			lines[row] = line + " " + scrollbarStyle.Render(string(barRune))
		}
	}
	return strings.Join(lines, "\n")
}

// Close terminates the spawned shell process and releases the pty. Safe
// to call even if the terminal was never started.
func (m Model) Close() error {
	if m.pty == nil {
		return nil
	}
	return m.pty.Close()
}

// encodeKey translates a Bubble Tea key event into the raw byte sequence a
// real terminal would send for that key, for forwarding to a pty's stdin.
// Returns nil for keys with no defined terminal encoding.
//
// Verified against bubbletea v1.3.10's key.go: KeyType values 0-127 for
// KeyCtrlA..KeyCtrlZ, the Ctrl-punctuation variants, KeyEnter, KeyTab,
// KeyEsc, and KeyBackspace are literally defined as their ASCII
// control-code byte value (e.g. KeyCtrlC = 3, KeyEnter = 13,
// KeyBackspace = 127) — forwarding byte(msg.Type) for that whole range is
// correct by construction, not a coincidence. Named cursor/navigation keys
// (KeyUp, KeyHome, etc.) are separate negative sentinel values needing
// their own standard xterm escape sequences (see this plan's Global
// Constraints for the one documented limitation: normal, not
// application, cursor-key mode).
func encodeKey(msg tea.KeyMsg) []byte {
	switch msg.Type {
	case tea.KeyRunes:
		return []byte(string(msg.Runes))
	case tea.KeySpace:
		return []byte(" ")
	case tea.KeyUp:
		return []byte("\x1b[A")
	case tea.KeyDown:
		return []byte("\x1b[B")
	case tea.KeyRight:
		return []byte("\x1b[C")
	case tea.KeyLeft:
		return []byte("\x1b[D")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyInsert:
		return []byte("\x1b[2~")
	}
	if msg.Type >= 0 && msg.Type <= 127 {
		return []byte{byte(msg.Type)}
	}
	return nil
}
