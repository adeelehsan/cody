package terminal

import (
	"bytes"
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

// maxHistory caps how many rows Model.history will ever hold — the same
// 10,000 lines the real vt.Emulator's own scrollback defaults to (see
// vt.DefaultScrollbackSize), which history now stands in for (see its
// own doc comment). Oldest entries are dropped first.
const maxHistory = 10000

// minEmuWidth/minEmuHeight floor the size the emulator and pty are ever
// actually resized to. A window drag really does pass through sizes
// where this pane has no room at all (the app floors the pane's size at
// 0, not 1), and a zero-size grid is where content gets destroyed past
// any recovery: there is nowhere to write the cursor's row back to, and
// nothing to capture from on the way back up. Below the floor the
// emulator simply stays at it, and View clips to the size actually
// requested.
const (
	minEmuWidth  = 1
	minEmuHeight = 1
)

// maxReflowTail bounds how many trailing history rows one SetSize call
// will re-wrap along with the live grid (see pullableTail) — a drag
// fires dozens of these a second. maxTailLineLookback bounds how much
// further back it will reach to start that tail on a logical-line
// boundary rather than mid-line.
const (
	maxReflowTail       = 4000
	maxTailLineLookback = 500
)

// eraseScrollbackSeq is ED 3, "erase saved lines"; clearScreenSeqs are
// the ways a shell clears the screen (`clear`, Ctrl+L: home + ED 2, or
// home + ED 0) — see Update.
var (
	eraseScrollbackSeq = []byte("\x1b[3J")
	clearScreenSeqs    = [][]byte{[]byte("\x1b[2J"), []byte("\x1b[H\x1b[J")}
)

// historyRowMeta is what reflowing a history row later needs to know
// about it beyond its text.
type historyRowMeta struct {
	// width is the pane width the row was laid out at — what "filled to
	// the edge" has to be measured against for THIS row (history mixes
	// rows from many widths) when its successor's continuation isn't
	// known.
	width int
	// continues says the row is a soft-wrap continuation of the row
	// before it; only meaningful when known (else the successor-of-a-
	// filled-row heuristic applies, as it does for an untracked live
	// row). History rows never change once they're here, so a known bit
	// stays good forever.
	continues, known bool
}

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
	// history is everything above the live grid, oldest first — one
	// chronological list this package owns outright, fed from two
	// directions: rows a resize pushed out of the live grid because
	// they no longer fit (writeReflowedRows), and rows real output
	// scrolled off the top, drained out of the emulator's own
	// scrollback right after every write (absorbOutput) so the
	// emulator's scrollback never holds anything between writes.
	//
	// One list, because only a single ordered list can be given BACK:
	// when the pane grows again, the rows a shrink pushed out return to
	// the live grid (see pullable) — and any output that scrolled off
	// in between is newer than them and has to come back below them.
	// While the emulator's scrollback and this package's resize
	// overflow were separate tiers, that case rendered out of order and
	// nothing could ever be pulled back at all: shrink the pane to
	// nothing and restore it, and the content was simply gone from
	// view, left behind in scrollback wrapped a few columns wide.
	//
	// historyMeta is parallel to history (see historyRowMeta).
	history     []string
	historyMeta []historyRowMeta
	// pullable is how many rows at the END of history are owed back to
	// the live grid when there's room: the rows resizes pushed out,
	// plus anything that scrolled off after them (it sits between them
	// and the live grid, so it has to come along). Each SetSize re-wraps
	// that tail together with the live rows and keeps whatever fits.
	// Ordinary scrollback — output that scrolled off with no resize
	// involved — is never pullable: a `clear` followed by a width
	// change must not drag old output back onto the screen.
	pullable int
	// emuW/emuH are the size the emulator and pty are actually at:
	// width/height floored at minEmuWidth/minEmuHeight (or Start's
	// 80x24 fallback while never sized). All reflow arithmetic runs on
	// these; width/height above stay what the caller asked for, and
	// clipped records that the two differ, so View must cut its output
	// down to the requested size itself.
	emuW, emuH int
	clipped    bool
	// rowContinues holds, for each of the current live rows (same
	// indexing as the grid itself, length == m.height when
	// trustworthy), whether that row is a soft-wrap continuation of
	// the row above it — as determined with certainty by this
	// package's own last reflow, not guessed from rendered text. nil
	// means "no trustworthy record for the current grid" (nothing has
	// been reflowed yet, or real output has since rewritten everything
	// it covered) — every consumer must treat nil exactly like "fall
	// back to isRowFilledToEdge's heuristic," so this can never make
	// behavior worse than before this field existed, only better. An
	// entry only counts while rowTracked says so (see below). See the
	// design spec at docs/superpowers/specs/2026-09-11-reflow-continuation-tracking-design.md.
	rowContinues []bool
	// rowWrittenWidths holds, for each of the current live rows (same
	// indexing/lifecycle as rowContinues), the exact display width this
	// package wrote that row at — ground truth, not an assumption. A
	// continuation row is not always exactly m.width wide: rewrapLogicalLine
	// can leave a row one column short when a trailing wide (double-width)
	// character cluster didn't fit and was carried whole to the next row
	// (see its own doc comment, pinned by TestRewrapLogicalLineNeverSplitsAWideCluster).
	// Recording the true written width (rather than assuming m.width) is
	// what lets SetSize's padding restoration below tell "the emulator
	// stripped a trailing space" apart from "this row was correctly one
	// column short" — conflating the two corrupts wide-character content
	// by inserting a space that was never there.
	rowWrittenWidths []int
	// rowTracked says which entries of rowContinues/rowWrittenWidths are
	// still trustworthy (same indexing and length; all three are nil
	// together). writeReflowedRows marks every row tracked; real output
	// then un-tracks only the rows it actually changed (see
	// retrackRowsAfterOutput) rather than discarding the whole record.
	// That distinction is what lets the record survive a real drag at
	// all: every pty resize delivers SIGWINCH, and an interactive shell
	// answers each one by redrawing its prompt — output that arrives
	// between EVERY pair of SetSize calls, but only ever rewrites the
	// prompt's own rows. An untracked row falls back to the
	// isRowFilledToEdge heuristic, exactly like a nil record does for
	// every row. See
	// docs/superpowers/specs/2026-09-20-reflow-tracking-survives-output-design.md.
	rowTracked []bool
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
// writeReflowedRows pushes whatever doesn't fit the new height out into
// history, oldest first, always keeping the NEWEST rows (including
// whichever holds the cursor) live — see its own doc comment. This one
// direction covers a pure width change, a pure height change, and both
// together.
//
// What a resize pushes out, a later resize gives back: those rows (see
// pullable) rejoin the computation on every subsequent call, re-wrapped
// at the new width together with the live rows as the one continuous
// run of lines they are, and return to the live grid as soon as they
// fit again. Without that, shrinking the pane far enough and restoring
// it left the content gone from view for good — stranded in scrollback,
// wrapped at whatever few columns the pane had passed through.
//
// The emulator and pty are never actually sized below
// minEmuWidth x minEmuHeight (see those constants): below that the
// requested size is only recorded, and View clips to it.
//
// Every resize recomputes fresh from the CURRENT grid plus that tail of
// history — there is no snapshot to go stale, no "is this still the
// same gesture" tracking needed, which is what makes a real multi-step
// drag (many small SetSize calls, one per intermediate size the OS
// reports) and a single big jump covering the same total resize
// naturally produce the same result.
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
	if width == m.width && height == m.height {
		return m
	}
	m.width, m.height = width, height
	if m.emu == nil {
		return m
	}
	emuW, emuH := max(width, minEmuWidth), max(height, minEmuHeight)
	m.clipped = width < emuW || height < emuH
	if emuW == m.emuW && emuH == m.emuH {
		return m // only the clip changed — see minEmuWidth
	}
	doReflow := !m.emu.IsAltScreen()
	var newRows []string
	var newCursorRow, newCursorCol int
	var newContinues []bool
	if doReflow {
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
		// Resolve "does row i continue row i-1" for every captured row:
		// the tracked record wins wherever it's still trustworthy (see
		// rowTracked), the isRowFilledToEdge heuristic covers every other
		// row — fresh output, or rows real output has rewritten since the
		// last reflow. Resolved here rather than inside reflowRows so the
		// pass-through branch below stores the same answer: handing
		// writeReflowedRows a nil/partial record there would have it
		// record "not a continuation" as certain for rows nobody ever
		// looked at, and the next width change would then refuse to
		// rejoin a genuinely wrapped line.
		//
		// The real vt.Emulator strips trailing blank cells when it
		// renders a row back to a string — indistinguishable, cell by
		// cell, from a row that just never reached the edge (see
		// reflow.go's design notes). Whenever the record knows row i
		// continues row i-1, row i-1 was written by this package's own
		// last reflow at the width recorded in m.rowWrittenWidths[i-1] —
		// NOT necessarily the pane width itself: a continuation row can
		// legitimately be one column short of it when a trailing
		// wide (double-width) character cluster didn't fit and
		// rewrapLogicalLine carried it whole to the next row instead of
		// splitting it (see rewrapLogicalLine's own doc comment and
		// TestRewrapLogicalLineNeverSplitsAWideCluster). Comparing against
		// the RECORDED true width, rather than assuming the pane width,
		// is what tells "the emulator stripped a trailing space"
		// (shortfall vs. the recorded width) apart from "this row was
		// correctly one column short already" (no shortfall vs. the
		// recorded width) — conflating the two would corrupt
		// wide-character content by padding in a space that was never
		// there.
		known := make([]bool, len(oldRows))
		for i := 1; i < len(oldRows); i++ {
			if i >= len(m.rowTracked) || !m.rowTracked[i] {
				known[i] = isRowFilledToEdge(oldRows[i-1], m.emuW)
				continue
			}
			known[i] = m.rowContinues[i]
			if !known[i] {
				continue
			}
			if pad := m.rowWrittenWidths[i-1] - ansi.StringWidth(oldRows[i-1]); pad > 0 {
				oldRows[i-1] += strings.Repeat(" ", pad)
			}
		}
		// Row 0's bit is a claim about the last row of history. It only
		// takes effect when that row is part of this reflow (a non-empty
		// tail below) — but it's resolved regardless, so a reflow that
		// doesn't reach back that far still hands it on intact.
		if len(known) > 0 {
			if len(m.rowTracked) > 0 && m.rowTracked[0] {
				known[0] = m.rowContinues[0]
			} else {
				known[0] = m.historyContinues(len(m.history))
			}
		}
		// The rows earlier resizes pushed out come back into the
		// computation (see pullable): re-wrapped at the new width
		// together with the live rows, as the one continuous run of
		// lines they are, with writeReflowedRows then keeping whatever
		// fits the new height live and pushing the rest back out.
		//
		// A width change re-wraps everything owed back (up to
		// maxReflowTail); a height-only change can't use more than
		// roughly a screenful of it, and re-wrapping rows at an
		// unchanged width just reproduces them — dragging the pane's
		// top edge shouldn't cost thousands of rows a step.
		tailLimit := maxReflowTail
		if emuW == m.emuW {
			tailLimit = min(tailLimit, 2*emuH)
		}
		tail, tailKnown, tailStart := m.pullableTail(tailLimit)
		if emuW != m.emuW || len(tail) > 0 {
			rows := append(append([]string{}, tail...), oldRows...)
			rowsKnown := append(append([]bool{}, tailKnown...), known...)
			newRows, newCursorRow, newCursorCol, newContinues = reflowRows(rows, m.emuW, emuW, cy+len(tail), cx, rowsKnown)
			if len(tail) == 0 && len(newContinues) > 0 {
				newContinues[0] = known[0]
			}
		} else {
			// Width didn't change and nothing is owed back — nothing to
			// rewrap. The old rows, cursor position, and resolved
			// continuation bits all pass through unchanged;
			// writeReflowedRows below still handles a height change on
			// its own (pushing excess into history if the new height is
			// smaller).
			newRows, newCursorRow, newCursorCol = oldRows, cy, cx
			newContinues = known
		}
		if removed := len(m.history) - tailStart; removed > 0 {
			m = m.alignHistoryMeta()
			m.history, m.historyMeta = m.history[:tailStart], m.historyMeta[:tailStart]
			m.pullable = max(0, m.pullable-removed)
			// A paused viewport is measured back from the live tail,
			// which these rows just rejoined; writeReflowedRows adds
			// back whatever it pushes out again. Never all the way to 0
			// here — that would silently un-pause it mid-computation.
			if m.scrollOffset > 0 {
				m.scrollOffset = max(1, m.scrollOffset-removed)
			}
		}
	} else {
		// The grid is about to be resized with no reflow of its own
		// (alt screen up) — whatever the record says about row
		// positions can't be trusted to line up with it afterward.
		m.rowContinues, m.rowWrittenWidths, m.rowTracked = nil, nil, nil
	}
	if m.pty != nil {
		m.pty.Resize(emuW, emuH)
	}
	m.emu.Resize(emuW, emuH)
	m.emuW, m.emuH = emuW, emuH
	if doReflow {
		m = m.writeReflowedRows(newRows, newCursorRow, newCursorCol, emuH, newContinues)
	}
	return m
}

// historyContinues reports whether history row h is a soft-wrap
// continuation of row h-1: the known bit if there is one, otherwise the
// filled-to-the-edge heuristic against row h-1 at the width IT was laid
// out at. h == len(m.history) asks the same question about whatever
// follows history's last row (the live grid's first row).
func (m Model) historyContinues(h int) bool {
	if h <= 0 || h > len(m.history) {
		return false
	}
	if h < len(m.historyMeta) && m.historyMeta[h].known {
		return m.historyMeta[h].continues
	}
	if h-1 < len(m.historyMeta) && m.historyMeta[h-1].width > 0 {
		return isRowFilledToEdge(m.history[h-1], m.historyMeta[h-1].width)
	}
	return false
}

// pullableTail returns the trailing history rows this resize re-wraps
// along with the live grid (see pullable), their resolved continuation
// bits, and the history index they start at (len(m.history) if there
// are none). Capped at limit rows, then extended back — by at
// most maxTailLineLookback more — to start on a logical-line boundary:
// starting mid-line would re-wrap the tail end of a line as if it were
// a whole one and leave its head behind at the old width.
func (m Model) pullableTail(limit int) (rows []string, continues []bool, start int) {
	n := min(m.pullable, len(m.history), limit)
	if n <= 0 {
		return nil, nil, len(m.history)
	}
	start = len(m.history) - n
	for back := 0; start > 0 && back < maxTailLineLookback && m.historyContinues(start); back++ {
		start--
	}
	rows = m.history[start:]
	continues = make([]bool, len(rows))
	for i := 1; i < len(rows); i++ {
		continues[i] = m.historyContinues(start + i)
	}
	return rows, continues, start
}

// alignHistoryMeta pads historyMeta out to history's length with
// zero-value (unknown) entries, so the two can be sliced and appended
// in lockstep from here on regardless of how history was populated.
func (m Model) alignHistoryMeta() Model {
	for len(m.historyMeta) < len(m.history) {
		m.historyMeta = append(m.historyMeta, historyRowMeta{})
	}
	m.historyMeta = m.historyMeta[:len(m.history)]
	return m
}

// pushHistory appends rows (with their meta) to history, dropping the
// oldest rows past maxHistory, and reports how many rows history
// actually grew by.
func (m Model) pushHistory(rows []string, meta []historyRowMeta) (Model, int) {
	m = m.alignHistoryMeta()
	before := len(m.history)
	m.history = append(m.history, rows...)
	m.historyMeta = append(m.historyMeta, meta...)
	if over := len(m.history) - maxHistory; over > 0 {
		m.history, m.historyMeta = m.history[over:], m.historyMeta[over:]
		before -= over
	}
	return m, len(m.history) - max(before, 0)
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
// m.history (see its own doc comment) rather than discarded,
// mirroring how a real terminal pushes reflow overflow into scrollback:
// this is always an append, never a prepend, since reflow has no
// "continuing the same gesture" concept (see reflowRows' own doc
// comment) — every call's overflow is, by construction, a fresh batch
// relative to whatever history already holds.
//
// newContinues carries the known-continuation bits for newRows (see
// reflowRows' own doc comment) — shifted/trimmed here in lockstep with
// newRows whenever excess rows get evicted into history, then
// stored as the new m.rowContinues (padded with false for any row from
// len(newRows) through newHeight-1, since blank padding is never a
// continuation of anything). This is what lets the NEXT SetSize call
// trust this resize's own output instead of re-deriving it from
// rendered strings — see the design spec at
// docs/superpowers/specs/2026-09-11-reflow-continuation-tracking-design.md.
// m.rowWrittenWidths is derived the same way, from newRows AFTER the
// eviction shift below (so it naturally lines up with the same, already
// -shifted rows, with no separate slice to shift in parallel) — see its
// own field doc comment on Model for why the true written width, not
// m.width, must be what SetSize compares against later.
func (m Model) writeReflowedRows(newRows []string, newCursorRow, newCursorCol, newHeight int, newContinues []bool) Model {
	if excess := len(newRows) - newHeight; excess > 0 {
		meta := make([]historyRowMeta, excess)
		for i := range meta {
			meta[i] = historyRowMeta{width: m.emuW, continues: i < len(newContinues) && newContinues[i], known: true}
		}
		var grew int
		m, grew = m.pushHistory(newRows[:excess], meta)
		// Everything a resize pushes out is owed back when there's
		// room again — see pullable.
		m.pullable = min(m.pullable+excess, len(m.history))
		// Pin a paused viewport the same way Update's OutputMsg case
		// already does when real output grows the combined buffer
		// underneath it: the combined buffer (history + live) just grew
		// by however much of this batch actually stuck (after the
		// maxHistory trim may have dropped some of it from the front),
		// so scrollOffset must grow by the same amount, or
		// renderScrolledView's paused viewport silently drifts toward
		// the live tail even though the user never asked it to.
		if m.scrollOffset > 0 {
			m.scrollOffset += grew
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
	// cursorAfterRewrap keeps a cursor's columns past the end of its
	// row's rendered content (see its own doc comment) — m.emuW is
	// already the new width by the time this runs.
	if m.emuW > 0 && newCursorCol >= m.emuW {
		newCursorCol = m.emuW - 1
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
	if len(newContinues) > newHeight {
		newContinues = newContinues[:newHeight]
	}
	m.rowContinues = append([]bool(nil), newContinues...)
	for len(m.rowContinues) < newHeight {
		m.rowContinues = append(m.rowContinues, false)
	}
	// rowWrittenWidths records the TRUE display width each row of
	// newRows was actually written at — ground truth for SetSize's
	// padding-restoration on the NEXT resize (see its own doc comment
	// and rowWrittenWidths' own field comment on Model). Rows beyond
	// len(newRows) are blank padding, never a continuation of anything,
	// so width 0 there is never consulted.
	m.rowWrittenWidths = make([]int, newHeight)
	for i, row := range newRows {
		if i >= newHeight {
			break
		}
		m.rowWrittenWidths[i] = ansi.StringWidth(row)
	}
	m.rowTracked = make([]bool, newHeight)
	for i := range m.rowTracked {
		m.rowTracked[i] = true
	}
	m.emu.Write([]byte(buf.String()))
	return m
}

// absorbOutput runs right after a write of real output: it drains
// whatever that write scrolled into the emulator's scrollback over into
// history (see history's own doc comment — the emulator's scrollback
// never holds anything between writes), and carries the tracked wrap
// record (rowContinues/rowWrittenWidths/rowTracked) across the write,
// keeping it only for what the write left alone. beforeRows and
// beforeScrollbackLen are the live grid's rendered rows and the
// scrollback length captured immediately before the write; nil
// beforeRows means "nothing was captured" and drops the record. It
// reports how many rows the write scrolled off the top.
//
// This package has no visibility into what arbitrary output did to the
// grid, so "left alone" is judged purely from the outside. Lining the
// rows up is simple: the rows this write scrolled off, followed by the
// live grid after it, is the same run of rows as the grid before it,
// index for index. Row x's bit records whether the row ABOVE it wrapped
// into it — the same thing a real terminal stores as a "wrapped" flag
// on row x-1 — so it stays tracked iff row x-1's rendered string is
// identical before and after. Row x's own content changing doesn't
// matter, exactly as it doesn't to a real terminal's flag: typing at a
// prompt that sits under a full-width row mustn't hand that boundary
// back to the heuristic, which would false-join the two. A row above
// that was rewritten, appended to, cleared, or moved by a scroll region
// or reverse index simply stops matching and falls back to the
// heuristic — all any row had before this record existed. One rewritten
// with byte-identical content (the usual result of a shell redrawing
// its prompt on SIGWINCH) keeps its state, correctly: the same content
// in the same place wraps the same way. rowWrittenWidths[x] is only
// ever consulted through a tracked bit x+1, which this rule already
// ties to row x being unchanged.
//
// A row that scrolls off takes its state into history with it: its
// continuation bit as a known one, and — since it's drained as a
// RENDERED string, trailing blanks stripped — the blank a tracked wrap
// boundary ended on put back, exactly as SetSize does for live rows.
// Without that, a wrapped line scrolling off mid-drag would come back
// from history with the words either side of the wrap fused.
//
// (Because the scrollback is drained after every write, it never
// reaches the library's own cap, where its growth would stop being an
// exact count of what scrolled.) A shrinking scrollback (cleared) or
// the alt screen coming up drops the record outright.
func (m Model) absorbOutput(beforeRows []string, beforeScrollbackLen int) (Model, int) {
	total := m.emu.ScrollbackLen()
	scrolled := total - beforeScrollbackLen
	drained := make([]string, total)
	meta := make([]historyRowMeta, total)
	for i := range drained {
		drained[i] = m.emu.ScrollbackLine(i)
		meta[i].width = m.emuW
	}
	if total > 0 {
		m.emu.ClearScrollback()
	}

	var continues, tracked []bool
	var widths []int
	worthKeeping := false
	if m.rowTracked != nil && beforeRows != nil && scrolled >= 0 && scrolled <= total && !m.emu.IsAltScreen() {
		afterRows := strings.Split(m.emu.Render(), "\n")
		after := append(append([]string{}, drained[total-scrolled:]...), afterRows...)
		n := len(m.rowTracked)
		unchanged := func(x int) bool {
			return x < len(after) && x < len(beforeRows) && after[x] == beforeRows[x]
		}
		bitTracked := func(x int) bool {
			return x < n && m.rowTracked[x] && (x == 0 || unchanged(x-1))
		}
		for x := 0; x < scrolled; x++ {
			d := total - scrolled + x
			if bitTracked(x) {
				meta[d].known, meta[d].continues = true, m.rowContinues[x]
			}
			if bitTracked(x+1) && m.rowContinues[x+1] {
				if pad := m.rowWrittenWidths[x] - ansi.StringWidth(drained[d]); pad > 0 {
					drained[d] += strings.Repeat(" ", pad)
				}
			}
		}
		continues, widths, tracked = make([]bool, n), make([]int, n), make([]bool, n)
		for i := range n {
			x := i + scrolled
			if x >= n {
				break // scrolled in fresh at the bottom: nothing recorded
			}
			continues[i], widths[i], tracked[i] = m.rowContinues[x], m.rowWrittenWidths[x], bitTracked(x)
			// A blank row above tracks trivially (nothing wraps out of
			// it); a record holding nothing else isn't worth a
			// before/after render on every future write.
			if tracked[i] && ((i > 0 && strings.TrimSpace(afterRows[i-1]) != "") || (i == 0 && continues[i])) {
				worthKeeping = true
			}
		}
	}
	if worthKeeping {
		m.rowContinues, m.rowWrittenWidths, m.rowTracked = continues, widths, tracked
	} else {
		m.rowContinues, m.rowWrittenWidths, m.rowTracked = nil, nil, nil
	}

	var grew int
	m, grew = m.pushHistory(drained, meta)
	if m.pullable > 0 {
		// Newer than the rows a resize pushed out, and sitting between
		// them and the live grid: it comes back with them (see pullable).
		m.pullable = min(m.pullable+grew, len(m.history))
	}
	return m, max(scrolled, 0)
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
	if maxOffset := m.emu.ScrollbackLen() + len(m.history); m.scrollOffset > maxOffset {
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
	m.emuW, m.emuH = w, h
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
		beforeLen := m.emu.ScrollbackLen()
		// Only worth capturing while there's a tracked record to
		// preserve — a session that has never been resized (or whose
		// record has fully aged out) pays nothing extra per write.
		var beforeRows []string
		if m.rowTracked != nil && !wasAltScreen {
			beforeRows = strings.Split(m.emu.Render(), "\n")
		}
		m.emu.Write(msg.data)
		if bytes.Contains(msg.data, eraseScrollbackSeq) {
			// ED 3 erases the emulator's scrollback — which lives in
			// history now (see its doc comment), so honoring the
			// sequence is this package's job. Whatever this same write
			// scrolled off AFTER the erase is still in the emulator's
			// scrollback for absorbOutput to pick up below.
			m.history, m.historyMeta, m.pullable = nil, nil, 0
			m.scrollOffset = 0
		}
		for _, seq := range clearScreenSeqs {
			if bytes.Contains(msg.data, seq) {
				// The user cleared the screen: whatever earlier resizes
				// pushed out is just scrollback now, not something a
				// later resize should put back in front of them.
				m.pullable = 0
				break
			}
		}
		var scrolled int
		m, scrolled = m.absorbOutput(beforeRows, beforeLen)
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
			m.scrollOffset += scrolled
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
	if m.clipped {
		// The emulator is never sized below minEmuWidth x minEmuHeight,
		// so the only requested sizes it can be bigger than are ones
		// with no room to show anything at all.
		return ""
	}
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
// by hand, oldest first: m.history (everything above the live grid — see
// its own doc comment), then the live screen's own rows. The emulator's
// own scrollback is read first, ahead of history, purely for
// completeness: Update drains it into history after every write, so in
// a running session it is always empty here. The live screen's
// individual rows come from splitting Render()'s own output on "\n" —
// safe because ANSI SGR/CSI escape sequences never contain a raw
// newline byte.
func (m Model) renderScrolledView() string {
	liveLines := strings.Split(m.emu.Render(), "\n")
	overflowLen := len(m.history)
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
			// Anything still in the emulator's own scrollback — normally
			// nothing (see this function's doc comment).
			line = m.emu.ScrollbackLine(i)
		case i-sbLen < overflowLen:
			line = m.history[i-sbLen]
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
