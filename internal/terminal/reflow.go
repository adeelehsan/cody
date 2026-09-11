package terminal

import "github.com/charmbracelet/x/ansi"

// isRowFilledToEdge reports whether row's rendered content occupies the
// terminal's full display width — the wrap-detection heuristic this
// package relies on (see groupIntoLogicalLines): a row that is NOT
// filled to the edge cannot have been soft-wrapped into the row below
// it, since the terminal only wraps when a row is completely full.
// width must be > 0 — callers (SetSize) already guard this.
func isRowFilledToEdge(row string, width int) bool {
	return ansi.StringWidth(row) >= width
}

// groupIntoLogicalLines groups physical rows (at the given width) into
// logical lines: row i+1 continues logical line L iff row i is filled
// to the edge (see isRowFilledToEdge's own doc comment for the known
// false-join limitation this implies). Each returned line is the
// concatenation of its physical rows' content, in order.
// physicalRowCounts[j] holds how many physical rows contributed to
// lines[j], in the same order — cursorOffset (see reflow.go) uses this
// to map a physical row index back to a logical line index.
//
// This is a thin wrapper over groupIntoLogicalLinesKnown with no
// override — every existing caller keeps this exact heuristic-only
// behavior unchanged.
func groupIntoLogicalLines(rows []string, width int) (lines []string, physicalRowCounts []int) {
	return groupIntoLogicalLinesKnown(rows, width, nil)
}

// groupIntoLogicalLinesKnown is groupIntoLogicalLines with an optional
// authoritative override: when known[i] is available (known != nil and
// i < len(known)), it is trusted outright as "row i continues row i-1"
// instead of being computed from isRowFilledToEdge(rows[i-1], width) —
// this is how reflowRows avoids repeating a rendered-string guess for
// rows it already reflowed once itself (see reflowRows' doc comment
// and the design spec's "self-tracked continuation bits" section). A
// known slice shorter than rows (or nil) falls back to the heuristic
// for every row past its end — never a hard error, always a graceful
// degrade to prior behavior.
func groupIntoLogicalLinesKnown(rows []string, width int, known []bool) (lines []string, physicalRowCounts []int) {
	for i, row := range rows {
		continues := i > 0 && isRowFilledToEdge(rows[i-1], width)
		if known != nil && i < len(known) {
			continues = known[i]
		}
		if i == 0 || !continues {
			lines = append(lines, row)
			physicalRowCounts = append(physicalRowCounts, 1)
			continue
		}
		lines[len(lines)-1] += row
		physicalRowCounts[len(physicalRowCounts)-1]++
	}
	return lines, physicalRowCounts
}

// rewrapLogicalLine re-wraps a logical line's content at newWidth,
// returning its new physical rows. Built on ansi.Truncate/TruncateLeft
// (already an indirect dependency via lipgloss, which lipgloss.Width
// itself is built on) rather than a hand-rolled grapheme-walking loop:
// Truncate is already ANSI-aware and wide-character-safe, confirmed by
// reading its implementation to drop a grapheme cluster entirely — never
// split it — if including it would push the accumulated width past the
// limit. Advancing by ansi.StringWidth(row) (not newWidth) after each
// row is what makes the non-split rule automatic: in the one case where
// Truncate dropped a trailing wide cluster, row's width is
// newWidth-1, so the next TruncateLeft naturally leaves that cluster as
// the first thing in the next row instead of skipping or duplicating it.
//
// Loop termination rests entirely on the zero-progress guard below:
// every iteration that does NOT break consumed at least one display
// column, and TruncateLeft(remaining, consumed, "") drops exactly those
// columns, so remaining's display width strictly decreases each time
// and must reach the guard's zero-width condition (or "") in finitely
// many steps.
func rewrapLogicalLine(line string, newWidth int) []string {
	if line == "" {
		return []string{""}
	}
	var rows []string
	remaining := line
	for remaining != "" {
		row := ansi.Truncate(remaining, newWidth, "")
		consumed := ansi.StringWidth(row)
		if consumed == 0 {
			// No VISIBLE progress this iteration, so continuing would
			// loop forever: TruncateLeft(remaining, 0, "") returns
			// remaining unchanged. Two distinct ways to land here, and
			// checking the row's display WIDTH (not just row == "")
			// catches both:
			//
			//  1. row is a non-empty, zero-width string — pure escape
			//     sequences with no visible cell. Truncate/TruncateLeft
			//     re-emit the ACTIVE SGR/OSC-8 state even when nothing
			//     visible is left, so the final remainder of ANY styled
			//     logical line (virtually every real shell prompt) looks
			//     like this. Attach that remainder to the PREVIOUS row
			//     rather than dropping it — a trailing SGR reset still
			//     matters, and being zero-width it can't push that row
			//     past newWidth.
			//  2. row is empty: not even one cluster fits at this width
			//     (e.g. newWidth==1 with a double-width character next).
			//     The remainder still has visible content, so it gets
			//     dumped as a single overflowing row of its own rather
			//     than smeared onto the previous one — an extreme edge
			//     case, not the target scenario, so "doesn't hang" is
			//     the bar, not "doesn't overflow visually."
			//
			// With no previous row to attach to (case 1 on the very
			// first iteration), the remainder becomes its own row —
			// there is nothing else to do with it.
			if len(rows) > 0 && ansi.StringWidth(remaining) == 0 {
				rows[len(rows)-1] += remaining
			} else {
				rows = append(rows, remaining)
			}
			break
		}
		rows = append(rows, row)
		remaining = ansi.TruncateLeft(remaining, consumed, "")
	}
	return rows
}

// cursorOffset returns the display-column offset of the cursor at
// (cursorRow, cursorCol) — both physical, 0-indexed — within its
// logical line, given rows (the physical rows at the OLD width) and
// physicalRowCounts (from groupIntoLogicalLines against those same
// rows). logicalLineIndex is which of groupIntoLogicalLines' returned
// lines the cursor falls in.
func cursorOffset(rows []string, physicalRowCounts []int, cursorRow, cursorCol int) (logicalLineIndex, offset int) {
	rowIdx := 0
	for li, count := range physicalRowCounts {
		if cursorRow < rowIdx+count {
			within := cursorRow - rowIdx
			off := 0
			for k := 0; k < within; k++ {
				off += ansi.StringWidth(rows[rowIdx+k])
			}
			return li, off + cursorCol
		}
		rowIdx += count
	}
	// cursorRow is beyond every known row — clamp to the end of the
	// last logical line rather than panic; SetSize's own bounds should
	// prevent this in practice, but this keeps the function total.
	last := len(physicalRowCounts) - 1
	if last < 0 {
		return 0, 0
	}
	off := 0
	for k := rowIdx - physicalRowCounts[last]; k < rowIdx; k++ {
		off += ansi.StringWidth(rows[k])
	}
	return last, off
}

// cursorAfterRewrap returns the (row, col) — both 0-indexed — that
// offset (from cursorOffset) lands at within newRows (the output of
// rewrapLogicalLine for the SAME logical line the offset was computed
// against).
//
// The row-boundary comparison is strict (offset < w, not <=): an offset
// exactly equal to a row's width is the START of the NEXT row (column
// 0), not one-past-the-end of the current one — a cursor legitimately
// produces this exact offset whenever it sits at column 0 of a
// continuation row (cursorOffset sums the full width of every prior
// physical row before adding its own column). Using <= placed such a
// cursor one row too high; falling through to decrement offset by w and
// continue naturally lands it at (nextRow, 0) instead. The fallback
// after the loop is unchanged: offset landing exactly at the end of the
// LAST row (no next row to fall through to) still returns
// (last, width) — the correct "end of content" position.
func cursorAfterRewrap(newRows []string, offset int) (row, col int) {
	for i, r := range newRows {
		w := ansi.StringWidth(r)
		if offset < w {
			return i, offset
		}
		offset -= w
	}
	if len(newRows) == 0 {
		return 0, 0
	}
	last := len(newRows) - 1
	return last, ansi.StringWidth(newRows[last])
}

// reflowRows re-wraps every physical row in rows (at oldWidth) to
// newWidth, and maps (cursorRow, cursorCol) — both 0-indexed physical
// coordinates — into the new layout. This is the single entry point
// Model.SetSize calls; it is stateless (nothing here reads or writes
// any field that persists across calls) — every call re-derives
// entirely from the rows it's given, which is what makes this robust
// against a shell redrawing or scrolling mid-resize (see the design
// spec's §3.4): there is no earlier-moment snapshot for such a redraw
// to invalidate.
func reflowRows(rows []string, oldWidth, newWidth, cursorRow, cursorCol int) (newRows []string, newCursorRow, newCursorCol int) {
	lines, counts := groupIntoLogicalLines(rows, oldWidth)
	cursorLine, offset := cursorOffset(rows, counts, cursorRow, cursorCol)
	for li, line := range lines {
		rewrapped := rewrapLogicalLine(line, newWidth)
		if li == cursorLine {
			r, c := cursorAfterRewrap(rewrapped, offset)
			newCursorRow = len(newRows) + r
			newCursorCol = c
		}
		newRows = append(newRows, rewrapped...)
	}
	return newRows, newCursorRow, newCursorCol
}
