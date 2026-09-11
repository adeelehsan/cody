package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestIsRowFilledToEdgeAtExactWidth(t *testing.T) {
	if !isRowFilledToEdge("1234567890", 10) {
		t.Fatalf("got false, want true — row's rendered width exactly matches the terminal width")
	}
}

func TestIsRowFilledToEdgeWhenShorter(t *testing.T) {
	if isRowFilledToEdge("short", 10) {
		t.Fatalf("got true, want false — row's rendered width is less than the terminal width")
	}
}

func TestGroupIntoLogicalLinesJoinsAWrappedPair(t *testing.T) {
	lines, counts := groupIntoLogicalLines([]string{"1234567890", "abcde"}, 10)
	wantLines := []string{"1234567890abcde"}
	wantCounts := []int{2}
	if len(lines) != len(wantLines) || lines[0] != wantLines[0] {
		t.Fatalf("got lines=%q, want %q", lines, wantLines)
	}
	if len(counts) != len(wantCounts) || counts[0] != wantCounts[0] {
		t.Fatalf("got counts=%v, want %v", counts, wantCounts)
	}
}

func TestGroupIntoLogicalLinesKeepsShortRowsSeparate(t *testing.T) {
	lines, counts := groupIntoLogicalLines([]string{"short", "next"}, 10)
	wantLines := []string{"short", "next"}
	wantCounts := []int{1, 1}
	if len(lines) != 2 || lines[0] != wantLines[0] || lines[1] != wantLines[1] {
		t.Fatalf("got lines=%q, want %q", lines, wantLines)
	}
	if len(counts) != 2 || counts[0] != wantCounts[0] || counts[1] != wantCounts[1] {
		t.Fatalf("got counts=%v, want %v", counts, wantCounts)
	}
}

func TestGroupIntoLogicalLinesJoinsAThreeRowChain(t *testing.T) {
	lines, counts := groupIntoLogicalLines([]string{"1234567890", "1234567890", "end"}, 10)
	if len(lines) != 1 || lines[0] != "12345678901234567890end" {
		t.Fatalf("got lines=%q, want a single joined logical line", lines)
	}
	if len(counts) != 1 || counts[0] != 3 {
		t.Fatalf("got counts=%v, want [3]", counts)
	}
}

// TestGroupIntoLogicalLinesFalseJoinOnAFullWidthRow pins a KNOWN,
// documented limitation of the "filled to the edge" heuristic (see the
// design spec's §7/§8): a row that happens to fill the terminal's full
// width without actually being a soft-wrapped continuation — e.g. a
// box-drawing border the same width as the pane — gets incorrectly
// joined with the row below it. This is NOT fixed by this plan; the
// test exists so a future change can't silently make it worse without
// this test flagging it.
func TestGroupIntoLogicalLinesFalseJoinOnAFullWidthRow(t *testing.T) {
	lines, counts := groupIntoLogicalLines([]string{"1234567890", "unrelated!"}, 10)
	if len(lines) != 1 || lines[0] != "1234567890unrelated!" {
		t.Fatalf("got lines=%q, want the two unrelated full-width rows incorrectly joined (pinning the known heuristic limitation, not fixing it)", lines)
	}
	if len(counts) != 1 || counts[0] != 2 {
		t.Fatalf("got counts=%v, want [2]", counts)
	}
}

// TestGroupIntoLogicalLinesMissesAWrapBoundaryEndingInBlank pins the
// INVERSE of the limitation above, and is likewise a KNOWN, ACCEPTED
// limitation rather than a bug awaiting a fix (see the design spec's
// §8): a row that genuinely DID soft-wrap, but whose last cell happens
// to be blank, measures NARROWER than the full width — the underlying
// library's rendering strips trailing blank cells — so the heuristic
// reads it as "not a continuation" and never rejoins it on a later
// grow. This is common in prose, where a wrap point often lands right
// after a space. Verified against the real vt.Emulator: printing
// "abcde fgh ijk" at width 10 renders as "abcde fgh" / "ijk" (the
// boundary space at column 10 already gone from the rendered row), and
// growing back to width 40 leaves the two rows unjoined.
//
// The principled fix needs cell-level grid access to distinguish a
// blank cell inside a filled row from an absent one, which the design
// deliberately rules out (spec §2's "no new Emulator methods"
// constraint). The test exists so a future change can't silently make
// this worse unnoticed.
func TestGroupIntoLogicalLinesMissesAWrapBoundaryEndingInBlank(t *testing.T) {
	// "abcde fgh " filled all 10 columns when printed, but renders back
	// as the 9-column "abcde fgh".
	lines, counts := groupIntoLogicalLines([]string{"abcde fgh", "ijk"}, 10)
	wantLines := []string{"abcde fgh", "ijk"}
	if len(lines) != 2 || lines[0] != wantLines[0] || lines[1] != wantLines[1] {
		t.Fatalf("got lines=%q, want %q — the genuinely-wrapped pair left UNjoined (pinning the known heuristic limitation, not fixing it)", lines, wantLines)
	}
	if len(counts) != 2 || counts[0] != 1 || counts[1] != 1 {
		t.Fatalf("got counts=%v, want [1 1]", counts)
	}
}

func TestRewrapLogicalLineNarrowsWithoutLoss(t *testing.T) {
	got := rewrapLogicalLine("ABCDEFGHIJKL", 6)
	want := []string{"ABCDEF", "GHIJKL"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRewrapLogicalLineWidensBackToOneRow(t *testing.T) {
	got := rewrapLogicalLine("ABCDEF"+"GHIJKL", 40)
	want := []string{"ABCDEFGHIJKL"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRewrapLogicalLineEmptyProducesOneEmptyRow(t *testing.T) {
	got := rewrapLogicalLine("", 10)
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("got %q, want one empty row", got)
	}
}

// TestRewrapLogicalLineNeverSplitsAWideCluster hand-traces
// ansi.Truncate's own documented behavior: a double-width grapheme
// cluster that would land exactly on the last column is dropped
// entirely (not split) and carried over whole to the next row. "AB你好"
// rewrapped at width 3: "AB" fits (width 2); adding "你" (width 2)
// would make width 4 > 3, so it's dropped from row 1 and carried whole
// into row 2, which in turn can't also fit "好" (2+2=4>3), carrying it
// into row 3.
func TestRewrapLogicalLineNeverSplitsAWideCluster(t *testing.T) {
	got := rewrapLogicalLine("AB你好", 3)
	want := []string{"AB", "你", "好"}
	if len(got) != len(want) {
		t.Fatalf("got %d rows %q, want %d rows %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got rows %q, want %q", got, want)
		}
	}
}

// TestRewrapLogicalLineAtWidthOneWithWideCharacterDoesNotHang covers a
// degenerate edge case: newWidth=1 can never fit ANY double-width
// cluster. rewrapLogicalLine must still terminate (dumping the
// remainder as one overflowing row) rather than loop forever.
func TestRewrapLogicalLineAtWidthOneWithWideCharacterDoesNotHang(t *testing.T) {
	done := make(chan []string, 1)
	go func() { done <- rewrapLogicalLine("你好", 1) }()
	select {
	case got := <-done:
		if len(got) == 0 {
			t.Fatalf("got no rows, want at least one (even if overflowing)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rewrapLogicalLine hung — degenerate width guard is missing or broken")
	}
}

// TestRewrapLogicalLineWithStyledContentDoesNotHang covers the case
// that matters most in practice, since virtually every real shell
// prompt and command output is colorized: ansi.Truncate/TruncateLeft
// re-emit the ACTIVE SGR state even once no visible content is left, so
// the final remainder of a styled logical line is a non-empty but
// ZERO-WIDTH string (pure escape sequences). A progress guard that only
// checks for an EMPTY row never fires on it — ansi.StringWidth is 0, so
// ansi.TruncateLeft(remaining, 0, "") returns the remainder unchanged
// and the loop spins forever, appending a row per iteration until the
// process runs out of memory. Verified against the real pinned
// charmbracelet/x/ansi: "\x1b[1mbold\x1b[0m" truncated at width 10
// yields a remainder of "\x1b[1m\x1b[0m" that never shrinks.
func TestRewrapLogicalLineWithStyledContentDoesNotHang(t *testing.T) {
	done := make(chan []string, 1)
	go func() { done <- rewrapLogicalLine("\x1b[1mbold\x1b[0m", 10) }()
	select {
	case got := <-done:
		if len(got) != 1 {
			t.Fatalf("got %d rows %q, want exactly 1 — the content is only 4 columns wide and fits whole at width 10", len(got), got)
		}
		if ansi.Strip(got[0]) != "bold" {
			t.Fatalf("got visible text %q from row %q, want %q", ansi.Strip(got[0]), got[0], "bold")
		}
		if w := ansi.StringWidth(got[0]); w != 4 {
			t.Fatalf("got display width %d for row %q, want 4 — the zero-width remainder must not add visible columns", w, got[0])
		}
		if !strings.Contains(got[0], "\x1b[0m") {
			t.Fatalf("got row %q, want the trailing SGR reset carried through rather than stranded and dropped — without it the style bleeds into whatever renders next", got[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rewrapLogicalLine hung on ANSI-styled content — the loop's progress guard must treat a zero-WIDTH row as no progress, not just an empty one")
	}
}

// TestRewrapLogicalLineWithStyledContentAcrossMultipleRows is the same
// zero-width-remainder hazard on a styled line long enough to need
// several rows, with the style changing mid-line (the shape of a real
// colorized prompt): every row must carry its full width of visible
// content, nothing may be dropped, and the whole thing must terminate.
func TestRewrapLogicalLineWithStyledContentAcrossMultipleRows(t *testing.T) {
	const line = "\x1b[1;31mRED\x1b[32mGREEN\x1b[0mplain\x1b[4munder\x1b[0m" // 18 visible columns
	done := make(chan []string, 1)
	go func() { done <- rewrapLogicalLine(line, 6) }()
	select {
	case got := <-done:
		if len(got) != 3 {
			t.Fatalf("got %d rows %q, want 3 — 18 visible columns at width 6", len(got), got)
		}
		var visible strings.Builder
		for i, row := range got {
			if w := ansi.StringWidth(row); w != 6 {
				t.Fatalf("got display width %d for row %d (%q), want 6", w, i, row)
			}
			visible.WriteString(ansi.Strip(row))
		}
		if visible.String() != "REDGREENplainunder" {
			t.Fatalf("got visible text %q across the rewrapped rows %q, want %q — no styled content may be dropped", visible.String(), got, "REDGREENplainunder")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rewrapLogicalLine hung on multi-row ANSI-styled content — see TestRewrapLogicalLineWithStyledContentDoesNotHang")
	}
}

func TestCursorOffsetWithinFirstPhysicalRow(t *testing.T) {
	_, counts := groupIntoLogicalLines([]string{"short", "next"}, 10)
	li, off := cursorOffset([]string{"short", "next"}, counts, 0, 3)
	if li != 0 || off != 3 {
		t.Fatalf("got (line=%d, offset=%d), want (0, 3)", li, off)
	}
}

func TestCursorOffsetInSecondPhysicalRowOfAWrappedLine(t *testing.T) {
	rows := []string{"1234567890", "abcde"}
	_, counts := groupIntoLogicalLines(rows, 10)
	li, off := cursorOffset(rows, counts, 1, 2)
	// Row 0 contributes its full width (10) before row 1's own column
	// (2) — offset is measured from the logical line's own start.
	if li != 0 || off != 12 {
		t.Fatalf("got (line=%d, offset=%d), want (0, 12)", li, off)
	}
}

func TestCursorAfterRewrapLandsOnTheCorrectRowAndColumn(t *testing.T) {
	newRows := []string{"ABCDEF", "GHIJKL"}
	row, col := cursorAfterRewrap(newRows, 11)
	if row != 1 || col != 5 {
		t.Fatalf("got (row=%d, col=%d), want (1, 5) — offset 11 is 5 columns into the second row", row, col)
	}
}

// TestCursorAfterRewrapAtARowBoundaryLandsOnTheNextRowNotPastThePrevious
// covers an off-by-one at exactly a row boundary: an offset equal to a
// row's own width is the START of the NEXT row (column 0) — a cursor
// legitimately sitting at column 0 of a continuation row produces
// exactly this offset (cursorOffset sums the full width of every prior
// physical row before adding its own column, which is 0 here) — not
// one-past-the-end of the row before it.
func TestCursorAfterRewrapAtARowBoundaryLandsOnTheNextRowNotPastThePrevious(t *testing.T) {
	row, col := cursorAfterRewrap([]string{"ABC", "DEF"}, 3)
	if row != 1 || col != 0 {
		t.Fatalf("got (row=%d, col=%d), want (1, 0) — offset 3 is exactly the first row's width, so it's column 0 of the second row, not column 3 of the first", row, col)
	}
}

func TestReflowRowsNarrowsAndMapsCursorEndToEnd(t *testing.T) {
	// One logical line, "1234567890abcde" (15 columns), wrapped across
	// two physical rows at old width 10.
	rows := []string{"1234567890", "abcde"}
	newRows, newRow, newCol, _ := reflowRows(rows, 10, 6, 1, 1, nil) // cursor at row1, col1 = the 'b'
	wantRows := []string{"123456", "7890ab", "cde"}
	if len(newRows) != len(wantRows) {
		t.Fatalf("got %d rows %q, want %d rows %q", len(newRows), newRows, len(wantRows), wantRows)
	}
	for i := range wantRows {
		if newRows[i] != wantRows[i] {
			t.Fatalf("got rows %q, want %q", newRows, wantRows)
		}
	}
	// offset in the old logical line "1234567890abcde": row0 (width
	// 10) + col1 = 11 -> the 'b' character. In the new rows
	// ["123456","7890ab","cde"], offset 11 = row1 (width6) + row... let
	// the test assert whatever the implementation actually produces
	// for row/col, then hand-verify newRows[newRow][newCol] == 'b'.
	if newRows[newRow][newCol] != 'b' {
		t.Fatalf("got cursor landing on %q at (row=%d,col=%d), want it on 'b'", string(newRows[newRow][newCol]), newRow, newCol)
	}
}

func TestReflowRowsWidensAndRejoins(t *testing.T) {
	rows := []string{"ABCDEF", "GHIJKL"}                     // one logical line at old width 6
	newRows, newRow, newCol, _ := reflowRows(rows, 6, 40, 1, 5, nil) // cursor at row1 col5 = 'L'
	if len(newRows) != 1 || newRows[0] != "ABCDEFGHIJKL" {
		t.Fatalf("got %q, want a single rejoined row", newRows)
	}
	if newRows[newRow][newCol] != 'L' {
		t.Fatalf("got cursor landing on %q at (row=%d,col=%d), want it on 'L'", string(newRows[newRow][newCol]), newRow, newCol)
	}
}

func TestReflowRowsKeepsUnrelatedLogicalLinesSeparate(t *testing.T) {
	rows := []string{"short one", "short two", "short three"} // none filled to edge at width 20
	newRows, _, _, _ := reflowRows(rows, 20, 5, 0, 0, nil)
	// Each stays its own logical line, independently rewrapped — no
	// cross-line joining.
	if len(newRows) < 3 {
		t.Fatalf("got %d rows %q, want each of the 3 unrelated lines to still be present as separate wrapped groups", len(newRows), newRows)
	}
}

// TestGroupIntoLogicalLinesKnownOverrideForcesAJoin proves a `known`
// override wins over a heuristic that would NOT join these rows on its
// own (neither row is filled to the edge at width 10) — this is how
// reflowRows avoids re-guessing content it already reflowed once.
func TestGroupIntoLogicalLinesKnownOverrideForcesAJoin(t *testing.T) {
	lines, counts := groupIntoLogicalLinesKnown([]string{"short", "next"}, 10, []bool{false, true})
	wantLines := []string{"shortnext"}
	if len(lines) != 1 || lines[0] != wantLines[0] {
		t.Fatalf("got lines=%q, want %q — known[1]=true must force a join the heuristic alone would not make", lines, wantLines)
	}
	if len(counts) != 1 || counts[0] != 2 {
		t.Fatalf("got counts=%v, want [2]", counts)
	}
}

// TestGroupIntoLogicalLinesKnownOverrideForcesASplit proves the other
// direction: a `known` override can force rows APART even when the
// heuristic alone would join them (both rows filled to the edge at
// width 10).
func TestGroupIntoLogicalLinesKnownOverrideForcesASplit(t *testing.T) {
	lines, counts := groupIntoLogicalLinesKnown([]string{"1234567890", "abcdefghij"}, 10, []bool{false, false})
	wantLines := []string{"1234567890", "abcdefghij"}
	if len(lines) != 2 || lines[0] != wantLines[0] || lines[1] != wantLines[1] {
		t.Fatalf("got lines=%q, want %q — known[1]=false must force a split the heuristic alone would not make", lines, wantLines)
	}
	if len(counts) != 2 || counts[0] != 1 || counts[1] != 1 {
		t.Fatalf("got counts=%v, want [1 1]", counts)
	}
}

// TestGroupIntoLogicalLinesKnownShorterThanRowsFallsBackPastItsEnd
// proves a `known` slice shorter than `rows` degrades gracefully: rows
// within its range are still overridden, rows past its end fall back
// to the heuristic.
func TestGroupIntoLogicalLinesKnownShorterThanRowsFallsBackPastItsEnd(t *testing.T) {
	// known only covers row 0 and row 1 (both false — no override
	// effect there since heuristic already says false at index 0
	// trivially and these are short rows anyway). Row 2 has no known
	// entry, so it falls back to isRowFilledToEdge(rows[1], 10), which
	// is false ("next" is not filled to width 10) -> row 2 stays
	// separate.
	lines, counts := groupIntoLogicalLinesKnown([]string{"short", "next", "third"}, 10, []bool{false, false})
	wantLines := []string{"short", "next", "third"}
	if len(lines) != 3 || lines[0] != wantLines[0] || lines[1] != wantLines[1] || lines[2] != wantLines[2] {
		t.Fatalf("got lines=%q, want %q", lines, wantLines)
	}
	if len(counts) != 3 {
		t.Fatalf("got counts=%v, want 3 entries of 1 each", counts)
	}
}

// TestGroupIntoLogicalLinesKnownNilReproducesExistingHeuristic proves
// nil known is IDENTICAL to calling groupIntoLogicalLines directly —
// this is the regression guard that keeps all 8 existing
// groupIntoLogicalLines callers correct with zero code changes.
func TestGroupIntoLogicalLinesKnownNilReproducesExistingHeuristic(t *testing.T) {
	cases := [][]string{
		{"1234567890", "abcde"},
		{"short", "next"},
		{"1234567890", "1234567890", "end"},
		{"abcde fgh", "ijk"},
	}
	for _, rows := range cases {
		wantLines, wantCounts := groupIntoLogicalLines(rows, 10)
		gotLines, gotCounts := groupIntoLogicalLinesKnown(rows, 10, nil)
		if len(gotLines) != len(wantLines) {
			t.Fatalf("rows=%q: got %d lines, want %d", rows, len(gotLines), len(wantLines))
		}
		for i := range wantLines {
			if gotLines[i] != wantLines[i] {
				t.Fatalf("rows=%q: got lines=%q, want %q (nil known must match groupIntoLogicalLines exactly)", rows, gotLines, wantLines)
			}
		}
		for i := range wantCounts {
			if gotCounts[i] != wantCounts[i] {
				t.Fatalf("rows=%q: got counts=%v, want %v", rows, gotCounts, wantCounts)
			}
		}
	}
}

// TestReflowRowsReturnsContinuationBitsForItsOwnOutput proves reflowRows
// reports, for the rows it just produced, which ones are continuations
// of the row above — the FIRST physical row of every rewrapped logical
// line is false, every row after it within that same line is true.
// This is the record Model.SetSize persists so the NEXT resize doesn't
// have to re-guess this same content from its rendered strings.
func TestReflowRowsReturnsContinuationBitsForItsOwnOutput(t *testing.T) {
	// One logical line, "1234567890abcde" (15 columns), rewrapped at
	// width 6 into 3 physical rows: "123456"/"7890ab"/"cde" -> [false, true, true].
	rows := []string{"1234567890", "abcde"}
	newRows, _, _, newContinues := reflowRows(rows, 10, 6, 0, 0, nil)
	if len(newRows) != 3 {
		t.Fatalf("got %d rows %q, want 3", len(newRows), newRows)
	}
	wantContinues := []bool{false, true, true}
	if len(newContinues) != len(wantContinues) {
		t.Fatalf("got newContinues=%v, want %v", newContinues, wantContinues)
	}
	for i := range wantContinues {
		if newContinues[i] != wantContinues[i] {
			t.Fatalf("got newContinues=%v, want %v", newContinues, wantContinues)
		}
	}
}

// TestReflowRowsReturnsContinuationBitsAcrossTwoLogicalLines proves the
// false/true pattern restarts at false for each NEW logical line, not
// just once for the whole batch.
func TestReflowRowsReturnsContinuationBitsAcrossTwoLogicalLines(t *testing.T) {
	// Two unrelated logical lines (neither filled to edge at width 20),
	// each independently rewrapped at width 5: "short one" (9 cols) ->
	// "short"/" one" (2 rows), "short two" (9 cols) -> "short"/" two" (2 rows).
	rows := []string{"short one", "short two"}
	newRows, _, _, newContinues := reflowRows(rows, 20, 5, 0, 0, nil)
	if len(newRows) != 4 {
		t.Fatalf("got %d rows %q, want 4", len(newRows), newRows)
	}
	wantContinues := []bool{false, true, false, true}
	if len(newContinues) != len(wantContinues) {
		t.Fatalf("got newContinues=%v, want %v", newContinues, wantContinues)
	}
	for i := range wantContinues {
		if newContinues[i] != wantContinues[i] {
			t.Fatalf("got newContinues=%v, want %v", newContinues, wantContinues)
		}
	}
}

// TestReflowRowsConsumesAKnownOverride proves reflowRows actually
// passes `known` through to its internal grouping step, not just
// accepting and ignoring the parameter.
func TestReflowRowsConsumesAKnownOverride(t *testing.T) {
	// Without an override, "short"/"next" are NOT joined (neither
	// fills width 10) -> 2 independent logical lines, each passed
	// through unchanged at newWidth 10 -> 2 output rows.
	// WITH known=[false,true], they ARE forced into one 9-character
	// logical line "shortnext", which still fits in one row at newWidth 10.
	rows := []string{"short", "next"}
	newRows, _, _, _ := reflowRows(rows, 10, 10, 0, 0, []bool{false, true})
	if len(newRows) != 1 || newRows[0] != "shortnext" {
		t.Fatalf("got newRows=%q, want a single joined row %q — known override was not consumed", newRows, []string{"shortnext"})
	}
}
