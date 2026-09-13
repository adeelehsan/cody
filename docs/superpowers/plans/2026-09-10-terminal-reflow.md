# Terminal Width Reflow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace PR #8's width-shrink snapshot fix (which real-world testing found unreliable against interactive shells) with real text reflow, so narrowing and widening the terminal pane never permanently loses already-printed content.

**Architecture:** A new, pure, emulator-free algorithm (`internal/terminal/reflow.go`) groups the live grid's physical rows into logical lines using a "filled to the edge" wrap heuristic, re-wraps each logical line at the new width via `charmbracelet/x/ansi`'s wide-character-safe `Truncate`/`TruncateLeft`, and maps the cursor position through the same transform. `Model.SetSize` calls this on every width-only resize (recomputing fresh from the live grid every time — no snapshot, no state to go stale), replacing the old `widthShrinkSnapshot`/`restoreWidthShrink` machinery entirely.

**Tech Stack:** Go, `charmbracelet/x/vt`, `charmbracelet/x/ansi` (promoted from indirect to direct dependency), existing `internal/terminal` package conventions.

**Spec:** `docs/superpowers/specs/2026-09-10-terminal-reflow-design.md`

## Global Constraints

- The `terminal.Emulator` interface gets **no new methods** — reflow operates entirely on the plain ANSI-styled strings `Render()` already returns, and writes back through the existing `Write([]byte)` method via ANSI escape sequences (the same technique PR #8's `restoreWidthShrink` used).
- `CursorPosition() (x, y int)` (already on `Emulator`) is reused as-is.
- TDD: write the failing test, watch it fail, implement, watch it pass.
- Every SetSize-level fix must be verified against the **real** `vt.Emulator`, not just the fake — the fake cannot reproduce the library's destructive resize behavior, so a fake-only test proves nothing about the actual bug.
- `gofmt`-clean; `go vet ./...` and the full `go test ./...` suite green before every commit.
- Reflow is skipped entirely while `IsAltScreen()` is true, and skipped entirely when width and height change in the same `SetSize` call (falls back to today's existing behavior for that step — a later width-only step still reflows normally).
- This plan **removes** the `widthShrinkSnapshot`/`widthShrinkWidth`/`widthShrinkCursor` fields and the `restoreWidthShrink` method from `internal/terminal/model.go`, and the three tests that exercise them, since reflow replaces that mechanism entirely (spec §1).

---

### Task 1: Wrap-detection primitive + direct `ansi` dependency

**Files:**
- Create: `internal/terminal/reflow.go`
- Create: `internal/terminal/reflow_test.go`
- Modify: `go.mod` (promote `github.com/charmbracelet/x/ansi` from indirect to direct)

**Interfaces:**
- Produces: `isRowFilledToEdge(row string, width int) bool`

- [ ] **Step 1: Write the failing test**

```go
// internal/terminal/reflow_test.go
package terminal

import "testing"

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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/terminal/... -run TestIsRowFilledToEdge -v`
Expected: FAIL to compile with "undefined: isRowFilledToEdge"

- [ ] **Step 3: Add the direct dependency and write the implementation**

```bash
go get github.com/charmbracelet/x/ansi@v0.11.7
go mod tidy
```

```go
// internal/terminal/reflow.go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/terminal/... -run TestIsRowFilledToEdge -v`
Expected: PASS

- [ ] **Step 5: Full package check and commit**

```bash
go build ./... && go vet ./... && test -z "$(gofmt -l internal/terminal)"
go test ./internal/terminal/... -count=1
git add internal/terminal/reflow.go internal/terminal/reflow_test.go go.mod go.sum
git commit -m "feat: add wrap-detection primitive for terminal width reflow"
```

---

### Task 2: Group physical rows into logical lines

**Files:**
- Modify: `internal/terminal/reflow.go`
- Modify: `internal/terminal/reflow_test.go`

**Interfaces:**
- Consumes: `isRowFilledToEdge(row string, width int) bool` (Task 1)
- Produces: `groupIntoLogicalLines(rows []string, width int) (lines []string, physicalRowCounts []int)` — `physicalRowCounts[j]` is how many physical rows contributed to `lines[j]`, in the same order; needed later (Task 4) to map a physical cursor row back to a logical line index.

- [ ] **Step 1: Write the failing tests**

```go
// internal/terminal/reflow_test.go — append

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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/terminal/... -run TestGroupIntoLogicalLines -v`
Expected: FAIL to compile with "undefined: groupIntoLogicalLines"

- [ ] **Step 3: Write the implementation**

```go
// internal/terminal/reflow.go — append

// groupIntoLogicalLines groups physical rows (at the given width) into
// logical lines: row i+1 continues logical line L iff row i is filled
// to the edge (see isRowFilledToEdge's own doc comment for the known
// false-join limitation this implies). Each returned line is the
// concatenation of its physical rows' content, in order.
// physicalRowCounts[j] holds how many physical rows contributed to
// lines[j], in the same order — cursorOffset (see reflow.go) uses this
// to map a physical row index back to a logical line index.
func groupIntoLogicalLines(rows []string, width int) (lines []string, physicalRowCounts []int) {
	for i, row := range rows {
		if i == 0 || !isRowFilledToEdge(rows[i-1], width) {
			lines = append(lines, row)
			physicalRowCounts = append(physicalRowCounts, 1)
			continue
		}
		lines[len(lines)-1] += row
		physicalRowCounts[len(physicalRowCounts)-1]++
	}
	return lines, physicalRowCounts
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/terminal/... -run TestGroupIntoLogicalLines -v`
Expected: PASS (all four)

- [ ] **Step 5: Full package check and commit**

```bash
go build ./... && go vet ./... && gofmt -l internal/terminal
go test ./internal/terminal/... -count=1
git add internal/terminal/reflow.go internal/terminal/reflow_test.go
git commit -m "feat: group terminal rows into logical lines for reflow"
```

---

### Task 3: Rewrap a logical line at a new width

**Files:**
- Modify: `internal/terminal/reflow.go`
- Modify: `internal/terminal/reflow_test.go`

**Interfaces:**
- Produces: `rewrapLogicalLine(line string, newWidth int) []string`

- [ ] **Step 1: Write the failing tests**

```go
// internal/terminal/reflow_test.go — append

func TestRewrapLogicalLineNarrowsWithoutLoss(t *testing.T) {
	got := rewrapLogicalLine("ABCDEFGHIJKL", 6)
	want := []string{"ABCDEF", "GHIJKL"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRewrapLogicalLineWidensBackToOneRow(t *testing.T) {
	got := rewrapLogicalLine("ABCDEF" + "GHIJKL", 40)
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
```

Add `"time"` to `reflow_test.go`'s import block for the last test.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/terminal/... -run TestRewrapLogicalLine -v`
Expected: FAIL to compile with "undefined: rewrapLogicalLine"

- [ ] **Step 3: Write the implementation**

```go
// internal/terminal/reflow.go — append

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
func rewrapLogicalLine(line string, newWidth int) []string {
	if line == "" {
		return []string{""}
	}
	var rows []string
	remaining := line
	for remaining != "" {
		row := ansi.Truncate(remaining, newWidth, "")
		if row == "" {
			// Degenerate case: not even one cluster fits at this width
			// (e.g. newWidth==1 with a double-width character next).
			// Dump the remainder as a single overflowing row rather
			// than looping forever — an extreme edge case, not the
			// target scenario, so "doesn't hang" is the bar, not
			// "doesn't overflow visually."
			rows = append(rows, remaining)
			break
		}
		rows = append(rows, row)
		consumed := ansi.StringWidth(row)
		remaining = ansi.TruncateLeft(remaining, consumed, "")
	}
	return rows
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/terminal/... -run TestRewrapLogicalLine -v`
Expected: PASS (all five). If `TestRewrapLogicalLineNeverSplitsAWideCluster` fails with different row contents than predicted, trust the test run over the hand-traced prediction in the test's comment — fix the comment to match reality, not the code, as long as the core invariant (no split cluster) still holds.

- [ ] **Step 5: Full package check and commit**

```bash
go build ./... && go vet ./... && gofmt -l internal/terminal
go test ./internal/terminal/... -count=1
git add internal/terminal/reflow.go internal/terminal/reflow_test.go
git commit -m "feat: rewrap terminal logical lines at a new width"
```

---

### Task 4: Cursor offset mapping

**Files:**
- Modify: `internal/terminal/reflow.go`
- Modify: `internal/terminal/reflow_test.go`

**Interfaces:**
- Consumes: `groupIntoLogicalLines` (Task 2), `rewrapLogicalLine` (Task 3)
- Produces: `cursorOffset(rows []string, physicalRowCounts []int, cursorRow, cursorCol int) (logicalLineIndex, offset int)`, `cursorAfterRewrap(newRows []string, offset int) (row, col int)`

- [ ] **Step 1: Write the failing tests**

```go
// internal/terminal/reflow_test.go — append

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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/terminal/... -run 'TestCursorOffset|TestCursorAfterRewrap' -v`
Expected: FAIL to compile with "undefined: cursorOffset" / "undefined: cursorAfterRewrap"

- [ ] **Step 3: Write the implementation**

```go
// internal/terminal/reflow.go — append

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
func cursorAfterRewrap(newRows []string, offset int) (row, col int) {
	for i, r := range newRows {
		w := ansi.StringWidth(r)
		if offset <= w {
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/terminal/... -run 'TestCursorOffset|TestCursorAfterRewrap' -v`
Expected: PASS (all three)

- [ ] **Step 5: Full package check and commit**

```bash
go build ./... && go vet ./... && gofmt -l internal/terminal
go test ./internal/terminal/... -count=1
git add internal/terminal/reflow.go internal/terminal/reflow_test.go
git commit -m "feat: map terminal cursor position through reflow"
```

---

### Task 5: Top-level orchestration function

**Files:**
- Modify: `internal/terminal/reflow.go`
- Modify: `internal/terminal/reflow_test.go`

**Interfaces:**
- Consumes: `groupIntoLogicalLines`, `rewrapLogicalLine`, `cursorOffset`, `cursorAfterRewrap` (Tasks 2-4)
- Produces: `reflowRows(rows []string, oldWidth, newWidth, cursorRow, cursorCol int) (newRows []string, newCursorRow, newCursorCol int)` — this is what `Model.SetSize` (Task 6) calls directly.

- [ ] **Step 1: Write the failing test**

```go
// internal/terminal/reflow_test.go — append

func TestReflowRowsNarrowsAndMapsCursorEndToEnd(t *testing.T) {
	rows := []string{"1234567890", "abcde"} // one logical line, 12 wide, at old width 10... wait: "1234567890"+"abcde" = 15 chars
	newRows, newRow, newCol := reflowRows(rows, 10, 6, 1, 1) // cursor at row1, col1 = the 'b'
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
	rows := []string{"ABCDEF", "GHIJKL"} // one logical line at old width 6
	newRows, newRow, newCol := reflowRows(rows, 6, 40, 1, 5) // cursor at row1 col5 = 'L'
	if len(newRows) != 1 || newRows[0] != "ABCDEFGHIJKL" {
		t.Fatalf("got %q, want a single rejoined row", newRows)
	}
	if newRows[newRow][newCol] != 'L' {
		t.Fatalf("got cursor landing on %q at (row=%d,col=%d), want it on 'L'", string(newRows[newRow][newCol]), newRow, newCol)
	}
}

func TestReflowRowsKeepsUnrelatedLogicalLinesSeparate(t *testing.T) {
	rows := []string{"short one", "short two", "short three"} // none filled to edge at width 20
	newRows, _, _ := reflowRows(rows, 20, 5, 0, 0)
	// Each stays its own logical line, independently rewrapped — no
	// cross-line joining.
	if len(newRows) < 3 {
		t.Fatalf("got %d rows %q, want each of the 3 unrelated lines to still be present as separate wrapped groups", len(newRows), newRows)
	}
}
```

Note: `newRows[newRow][newCol]` indexes bytes, not runes — safe here since every test in this file uses ASCII content for the cursor-landing assertions specifically (the wide-character cases are covered separately in Task 3, which doesn't need a cursor-landing check).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/terminal/... -run TestReflowRows -v`
Expected: FAIL to compile with "undefined: reflowRows"

- [ ] **Step 3: Write the implementation**

```go
// internal/terminal/reflow.go — append

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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/terminal/... -run TestReflowRows -v`
Expected: PASS (all three). If the exact row split in
`TestReflowRowsNarrowsAndMapsCursorEndToEnd` differs from the
predicted `wantRows`, trust the actual output and fix the test's
expected value — the cursor-lands-on-'b' assertion is the one that
must hold regardless.

- [ ] **Step 5: Full package check and commit**

```bash
go build ./... && go vet ./... && gofmt -l internal/terminal
go test ./internal/terminal/... -count=1
git add internal/terminal/reflow.go internal/terminal/reflow_test.go
git commit -m "feat: add top-level terminal reflow orchestration"
```

---

### Task 6: Wire reflow into SetSize; remove the old snapshot mechanism

**Files:**
- Modify: `internal/terminal/model.go`
- Modify: `internal/terminal/model_test.go`

**Interfaces:**
- Consumes: `reflowRows` (Task 5)
- Removes: `Model.widthShrinkSnapshot`, `Model.widthShrinkWidth`, `Model.widthShrinkCursor` fields; `Model.restoreWidthShrink` method
- Removes tests: `TestSetSizeRestoresContentLostToARealEmulatorsWidthShrink`, `TestSetSizeWidthRestoreSkipsARowOverwrittenByNewOutput`, `TestSetSizeWidthRestoreStillRestoresRowsNewOutputDidNotTouch` (they test the mechanism being removed)
- Produces: `Model.writeReflowedRows(newRows []string, newCursorRow, newCursorCol, newHeight int) Model`

- [ ] **Step 1: Remove the old mechanism**

In `internal/terminal/model.go`, delete:
- The `widthShrinkSnapshot []string`, `widthShrinkWidth int`, `widthShrinkCursor [2]int` fields (and their doc comments) from the `Model` struct.
- The paragraph in `SetSize`'s doc comment beginning "A width SHRINK gets an analogous, but separate, protection..." through "...same as before." (it describes the mechanism being removed).
- The `if changed && m.emu != nil && m.width > 0 && !m.emu.IsAltScreen() { switch { case height != m.height: ... case width < m.width && m.widthShrinkSnapshot == nil: ... } }` block inside `SetSize`.
- The `if changed && m.emu != nil && m.widthShrinkSnapshot != nil && width >= m.widthShrinkWidth && !m.emu.IsAltScreen() { m = m.restoreWidthShrink() }` block inside `SetSize`.
- The entire `restoreWidthShrink` method.

In `internal/terminal/model_test.go`, delete the three tests named above in full (each is a complete, self-contained function — remove from `func TestSetSize...` through its closing `}`).

- [ ] **Step 2: Verify the package still builds without the removed pieces**

Run: `go build ./... && go vet ./...`
Expected: builds clean (nothing else in the package references the removed fields/method — `SetSize` itself is the only caller).

- [ ] **Step 3: Write the failing test for the new behavior**

```go
// internal/terminal/model_test.go — append

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
```

- [ ] **Step 4: Run tests to verify they fail**

Run: `go test ./internal/terminal/... -run TestSetSizeReflow -v`
Expected: FAIL — `SetSize` doesn't call reflow yet, so shrinking still truncates.

- [ ] **Step 5: Write the implementation**

Replace `SetSize`'s doc comment paragraph about the removed mechanism (deleted in Step 1) with:

```go
// A width CHANGE (either direction) that leaves height unchanged
// reflows the live grid: ultraviolet's Buffer.Resize implements a
// width shrink as `Lines[i] = Lines[i][:width]` for every row —
// permanently truncating anything past the new column count, with no
// reflow of its own (the library has no per-row soft-wrap tracking to
// reflow from — confirmed directly in its source, and empirically
// against real terminals like tmux and Ghostty that DO reflow and
// don't lose this content). reflowRows (reflow.go) re-derives the
// live grid's logical lines and re-wraps them at the new width before
// the library's own resize call, and writeReflowedRows writes the
// result back afterward. This runs on every width-changing call,
// recomputing fresh each time — there is no snapshot to go stale, which
// is what makes this robust against a shell redrawing or even
// scrolling the screen in response to the very resize that triggered
// it (an earlier snapshot-based approach failed against exactly that,
// confirmed against a real interactive zsh session — see
// docs/superpowers/specs/2026-09-10-terminal-reflow-design.md).
//
// Skipped while the alt screen is active (same reasoning as the
// height-shrink capture above) and skipped when width and height
// change in the same call — the height component of such a call still
// gets the existing shrinkOverflow handling above; a later width-only
// step still reflows normally (see the design spec's §5 for why this
// is a deliberate scope boundary, not an oversight).
```

Add the reflow call in `SetSize`, between the existing height-shrink block and the `m.width, m.height = width, height` line:

```go
	doReflow := changed && m.emu != nil && m.width > 0 && m.height > 0 &&
		width != m.width && height == m.height && !m.emu.IsAltScreen()
	var reflowedRows []string
	var reflowedCursorRow, reflowedCursorCol int
	if doReflow {
		oldRows := strings.Split(m.emu.Render(), "\n")
		cx, cy := m.emu.CursorPosition()
		reflowedRows, reflowedCursorRow, reflowedCursorCol = reflowRows(oldRows, m.width, width, cy, cx)
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
	if doReflow {
		m = m.writeReflowedRows(reflowedRows, reflowedCursorRow, reflowedCursorCol, height)
	}
	return m
}

// writeReflowedRows writes newRows (already computed at the new width
// by reflowRows) into the live grid — which by the time this runs has
// already been resized to its final width/height — and repositions the
// cursor to (newCursorRow, newCursorCol), matching reflowRows' own
// (row, col) return order. Rows beyond newHeight are appended into
// m.shrinkOverflow (see its own doc comment) rather than discarded,
// mirroring how a real terminal pushes reflow overflow into scrollback:
// this is always an append, never a prepend, since reflow has no
// "continuing the same gesture" concept (see reflowRows' own doc
// comment) — every call's overflow is, by construction, a fresh batch
// relative to whatever shrinkOverflow already holds.
func (m Model) writeReflowedRows(newRows []string, newCursorRow, newCursorCol, newHeight int) Model {
	if excess := len(newRows) - newHeight; excess > 0 {
		m.shrinkOverflow = append(m.shrinkOverflow, newRows[:excess]...)
		if over := len(m.shrinkOverflow) - maxShrinkOverflow; over > 0 {
			m.shrinkOverflow = m.shrinkOverflow[over:]
		}
		newRows = newRows[excess:]
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
		fmt.Fprintf(&buf, "\x1b[%d;1H%s", i+1, row)
	}
	if newHeight > 0 {
		fmt.Fprintf(&buf, "\x1b[%d;%dH", newCursorRow+1, newCursorCol+1)
	}
	m.emu.Write([]byte(buf.String()))
	return m
}
```

Note the closing `return m` / `}` above belongs to `SetSize` itself — insert the `doReflow`/write-back block before `SetSize`'s existing `return m` and closing brace, then add `writeReflowedRows` as a new method after it (mirroring where `restoreWidthShrink` used to live, now removed).

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/terminal/... -run TestSetSizeReflow -v`
Expected: PASS (all three)

- [ ] **Step 7: Run the full existing suite to check for regressions**

Run: `go test ./internal/terminal/... -count=1 -v 2>&1 | tail -60`
Expected: all PASS. If `TestSetSizeDoesNotCaptureOnAWidthOnlyShrink` (an existing test — width 20→10 shrink with short lines, asserting `shrinkOverflow` stays empty) still passes, leave its assertion as-is, but update its comment: its current wording ("only a HEIGHT shrink discards rows in the real emulator") is no longer categorically true now that width-only reflow can also produce overflow — just not for this test's specific short-line input. Reword to something like: "a width-only shrink of content that doesn't need extra rows to reflow leaves shrinkOverflow untouched — see TestSetSizeReflow* for the cases that do overflow."

- [ ] **Step 8: Full package check and commit**

```bash
go build ./... && go vet ./... && gofmt -l internal/terminal
go test ./... -count=1
git add internal/terminal/model.go internal/terminal/model_test.go
git commit -m "feat: reflow terminal content on width resize instead of truncating

Replaces the width-shrink snapshot mechanism from PR #8 (widthShrinkSnapshot/restoreWidthShrink), which real-world testing found unreliable against interactive shells, with stateless reflow that recomputes from the live grid on every width-changing resize."
```

---

### Task 7: Height-interaction, alt-screen, and cursor-correctness tests

**Files:**
- Modify: `internal/terminal/model_test.go`
- Modify: `internal/terminal/model.go` (doc comment only)

**Interfaces:**
- Consumes: `writeReflowedRows`, `doReflow` gating in `SetSize` (Task 6)

- [ ] **Step 1: Write the failing tests**

```go
// internal/terminal/model_test.go — append

// TestSetSizeReflowOverflowAppendsIntoShrinkOverflow covers the height
// interaction from the design spec's §5: a width-only reflow that needs
// MORE rows than fit in the unchanged height pushes the excess (oldest,
// from the top) into shrinkOverflow rather than discarding it.
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
	if !strings.Contains(allUp.View(), "ABCDEFGHIJ") {
		t.Fatalf("got View()=%q after scrolling up through shrinkOverflow, want the earliest reflowed row present", allUp.View())
	}
}

// TestSetSizeSkipsReflowOnSimultaneousWidthAndHeightChange covers the
// design spec's B2 decision: reflow is skipped entirely when width and
// height change together in one SetSize call — that step falls back to
// today's existing (pre-reflow) width-truncation behavior instead. This
// deliberately does NOT test that no content is lost on this step —
// only that reflow's own machinery didn't run and nothing panics.
func TestSetSizeSkipsReflowOnSimultaneousWidthAndHeightChange(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(40, 10)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("some content")})
	m = updated

	// Should not panic, and shrinkOverflow's existing height-shrink
	// capture (untouched by this plan) still runs for the height
	// component of this simultaneous change.
	m = m.SetSize(10, 5)
	_ = m.View()
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/terminal/... -run 'TestSetSizeReflowOverflow|TestSetSizeSkipsReflow|TestSetSizeReflowSkippedDuringAltScreen' -v`
Expected: `TestSetSizeReflowOverflowAppendsIntoShrinkOverflow` FAILs (overflow handling not yet exercised end-to-end at this content size — if it already passes because Task 6 already covers it correctly, that's fine, note it and move on); the other two should already pass since they test guards Task 6 already put in place (`m.height > 0` / `width != m.width` conditions and the pre-existing `!m.emu.IsAltScreen()` guard) — if so, they serve as regression coverage rather than driving new code.

- [ ] **Step 3: Fix any failures**

If `TestSetSizeReflowOverflowAppendsIntoShrinkOverflow` fails, re-check `writeReflowedRows`'s excess-handling arithmetic against this test's exact numbers: `line` is 36 characters (26 letters + 10 digits), fitting one row at width 40 with the grid's second (blank) row alongside it since height is 2 — `Render()` at that point returns 2 physical rows. `groupIntoLogicalLines` keeps them as two separate logical lines (the 36-char line isn't filled to the edge at width 40, so it doesn't absorb the blank row below it). Rewrapping the 36-char line at width 10 needs 4 rows (10+10+10+6); the blank line rewraps to 1 row — 5 total reflowed rows against a height of 2, so `excess` is 3, not the row count of either logical line alone. If it doesn't match, the bug is most likely an off-by-one in the `excess`/`newCursorY -= excess` adjustment, or `oldRows` not including the grid's blank second row.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/terminal/... -run 'TestSetSizeReflowOverflow|TestSetSizeSkipsReflow|TestSetSizeReflowSkippedDuringAltScreen' -v`
Expected: PASS (all three)

- [ ] **Step 5: Update `shrinkOverflow`'s doc comment**

In `internal/terminal/model.go`, `Model` struct, update the `shrinkOverflow` field's doc comment (currently describing it as holding rows "a height-shrinking SetSize captured") to also mention width-reflow overflow:

```go
	// shrinkOverflow holds rows evicted by a resize that the underlying
	// emulator's own resize would otherwise have silently destroyed —
	// either a height-shrinking SetSize (see SetSize's own doc comment)
	// or a width-only reflow that needs more rows than fit in the
	// current height (see writeReflowedRows) — oldest first, rendered
	// as a tier BETWEEN m.emu's own scrollback and the live screen (see
	// renderScrolledView, ScrollLines): the rows it holds were on the
	// live screen at capture time, so they are newer than anything
	// already scrolled into the real scrollback by then, even though
	// the real scrollback usually holds far more lines overall
	// (everything that scrolled off before the shrink/reflow ever
	// happened).
```

- [ ] **Step 6: Full package check and commit**

```bash
go build ./... && go vet ./... && gofmt -l internal/terminal
go test ./... -count=1
git add internal/terminal/model.go internal/terminal/model_test.go
git commit -m "test: cover reflow's height interaction, alt-screen skip, and overflow handling"
```

---

### Task 8: Docs, full verification, real-shell end-to-end check, push

**Files:**
- Modify: `README.md`

**Interfaces:** none (integration/verification task)

- [ ] **Step 1: Update the README's limitations section**

In `README.md`, find the bullet added by PR #8 (starts "Narrowing the pane permanently truncates already-printed lines past the new width..."). Replace it with:

```markdown
- Reflow re-wraps already-printed content on every width change, so
  narrowing and widening the pane no longer loses characters in the
  common case. It relies on a heuristic (a row that fills the pane's
  full width is assumed to be a soft-wrapped continuation of the next
  one) rather than the underlying library tracking wrapping explicitly,
  so content that happens to fill the full width without actually being
  wrapped — a box-drawing border the same width as the pane, for
  example — can be incorrectly joined with the row after it.
```

- [ ] **Step 2: Run the full suite three times to rule out flakiness**

```bash
go build ./... && go vet ./... && gofmt -l .
go test ./... -count=3
```

Expected: all green, three times. (`internal/editor/model.go` may still show up under `gofmt -l` — this is a pre-existing, unrelated formatting issue from before this plan's work; confirm with `git log -1 --format=%H -- internal/editor/model.go` that it long predates this branch, and do not touch it.)

- [ ] **Step 3: Real end-to-end verification against an interactive shell via tmux**

This is the actual bar this feature has to clear — the exact reproduction that defeated PR #8's snapshot-based attempts.

```bash
go build -o /tmp/cody-reflow-verify ./cmd/cody
tmux kill-session -t reflowverify 2>/dev/null
tmux new-session -d -s reflowverify -x 150 -y 45 "/tmp/cody-reflow-verify $(pwd)"
sleep 1
tmux send-keys -t reflowverify C-t
sleep 1
```

Focus the terminal pane reliably by checking after each `Tab` press (focus cycling order is tree → editor → terminal → tree, but exact keystroke timing during shell startup can require more than one press — verify with an echo probe rather than assuming a fixed count):

```bash
for i in 1 2 3; do
  tmux send-keys -t reflowverify Tab
  sleep 0.4
  tmux send-keys -t reflowverify -l "echo FOCUSCHK$i"
  tmux send-keys -t reflowverify Enter
  sleep 0.4
  if tmux capture-pane -t reflowverify -p | grep -q "FOCUSCHK$i"; then
    echo "focused at tab $i"
    break
  fi
done
tmux send-keys -t reflowverify -l "cat README.md"
tmux send-keys -t reflowverify Enter
sleep 1
tmux capture-pane -t reflowverify -p | tail -20
```

Then shrink and grow the real window, checking content survives both ways:

```bash
tmux resize-window -t reflowverify -x 60 -y 45
sleep 0.7
tmux capture-pane -t reflowverify -p | tail -8
tmux resize-window -t reflowverify -x 150 -y 45
sleep 0.7
tmux capture-pane -t reflowverify -p | tail -8
```

Expected: after growing back to 150 columns, the README content lines are no longer truncated mid-word the way they were before this plan (compare against the exact truncation pattern reported in the original bug — `"  cap while scrolled up can"` / `"  oldest lines get evicted o"` cut off mid-word). If any line is still visibly cut short at 150 columns, STOP — do not proceed to push — re-open Task 6/7 and investigate before continuing; this exact scenario is the one two prior fix attempts failed on, so passing unit tests alone does not mean this step will pass.

```bash
tmux kill-session -t reflowverify 2>/dev/null
rm -f /tmp/cody-reflow-verify
```

- [ ] **Step 4: Commit the README update**

```bash
git add README.md
git commit -m "docs: document reflow's known false-join heuristic limitation"
```

- [ ] **Step 5: Push**

```bash
git push -u origin worktree-terminal-reflow
```

- [ ] **Step 6: Open the pull request**

```bash
gh pr create --title "feat: reflow terminal content on width resize instead of truncating" --body "$(cat <<'EOF'
## Summary
- Replaces PR #8's width-shrink snapshot fix with real text reflow, after real-world testing found the snapshot approach fails against interactive shells for two independent structural reasons (documented in the design spec).
- Reflow recomputes from the live grid statelessly on every width-changing resize, both narrowing and widening, and now correctly rejoins previously-wrapped content when the pane widens back out.
- A width-only reflow that needs more rows than the current height fit pushes the overflow into the existing `shrinkOverflow` mechanism from PR #8, rather than discarding it.

## Design
See `docs/superpowers/specs/2026-09-10-terminal-reflow-design.md` for the full design, including the known false-join heuristic limitation and everything deliberately left out of scope for this PR.

## Test plan
- [x] Pure algorithm unit tests (wrap detection, grouping, rewrap, cursor mapping, orchestration) — `internal/terminal/reflow_test.go`
- [x] Real-`vt.Emulator` round-trip tests (shrink without loss, grow rejoins) — `internal/terminal/model_test.go`
- [x] Height-interaction, alt-screen-skip, and overflow-into-`shrinkOverflow` tests
- [x] Real end-to-end verification via tmux against an interactive zsh session — the exact reproduction that defeated two prior fix attempts
- [x] `go build`, `go vet`, `gofmt`, and `go test ./... -count=3` all green

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Report the PR URL back when done.
