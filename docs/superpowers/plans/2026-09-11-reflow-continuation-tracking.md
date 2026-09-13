# Reflow Wrap-Continuation Self-Tracking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop `SetSize` from re-guessing "did this row wrap" from lossy rendered-string heuristics on every single resize call, which lets a single dropped word-boundary space early in a resize sequence compound into unrelated words getting fused together by the end of a real corner-drag.

**Architecture:** Add a `Model.rowContinues []bool` field that records, with certainty, which on-screen rows this package itself last wrote as wrap continuations. `reflowRows` both consumes this (instead of re-deriving from scratch) and produces the next generation of it. Any real PTY output invalidates it — the tracking only ever applies to a resize-only sequence with nothing typed in between, which is exactly the failure mode from the reported bug.

**Tech Stack:** Go, `charmbracelet/x/vt` (`vt.Emulator`), `charmbracelet/x/ansi`, existing `internal/terminal` package.

**Spec:** `docs/superpowers/specs/2026-09-11-reflow-continuation-tracking-design.md` (read this too — it explains WHY, including the ruled-out alternatives; this plan only covers HOW).

## Global Constraints

- `rewrapLogicalLine`, `cursorOffset`, `cursorAfterRewrap`, and `isRowFilledToEdge` in `internal/terminal/reflow.go` are NOT modified by this plan.
- `writeReflowedRows`' existing erase-per-row write logic (the `\x1b[%d;1H\x1b[K%s` loop), `shrinkOverflow`'s eviction direction/cap (`maxShrinkOverflow`), and its `scrollOffset`-pinning logic are NOT changed — this plan only threads one new parameter (`newContinues []bool`) through in lockstep with the existing `newRows` handling.
- `groupIntoLogicalLines(rows []string, width int)` keeps its exact existing 2-argument signature and behavior — all 8 existing call sites in `reflow_test.go` must keep passing with ZERO edits.
- No app-level layout code (`internal/app/*.go`) is touched.
- `nil` for `rowContinues`/`known`/`newContinues` must always be safe and must always degrade to today's exact pre-existing behavior — this plan can only make resize-content-preservation better, never worse, in any code path.

---

### Task 1: Add `groupIntoLogicalLinesKnown` and thread it through `reflowRows`

**Files:**
- Modify: `internal/terminal/reflow.go`
- Test: `internal/terminal/reflow_test.go`

**Interfaces:**
- Consumes: nothing from other tasks (this is the foundation task).
- Produces: `groupIntoLogicalLinesKnown(rows []string, width int, known []bool) (lines []string, physicalRowCounts []int)` and `reflowRows(rows []string, oldWidth, newWidth, cursorRow, cursorCol int, known []bool) (newRows []string, newCursorRow, newCursorCol int, newContinues []bool)` — Task 2 calls both of these by these exact names/signatures.

- [ ] **Step 1: Write the failing tests for `groupIntoLogicalLinesKnown`**

Add to `internal/terminal/reflow_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/terminal/... -run TestGroupIntoLogicalLinesKnown -v`
Expected: FAIL — `groupIntoLogicalLinesKnown` is undefined.

- [ ] **Step 3: Implement `groupIntoLogicalLinesKnown` and turn `groupIntoLogicalLines` into a thin wrapper**

Replace the existing `groupIntoLogicalLines` function in `internal/terminal/reflow.go` (currently lines 15-34) with:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/terminal/... -run TestGroupIntoLogicalLinesKnown -v`
Expected: PASS (all 4 new tests)

Run: `go test ./internal/terminal/... -run TestGroupIntoLogicalLines -v` (the original 5 existing tests, unchanged)
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/terminal/reflow.go internal/terminal/reflow_test.go
git commit -m "feat: add groupIntoLogicalLinesKnown, an overridable variant of groupIntoLogicalLines"
```

- [ ] **Step 6: Write the failing test for `reflowRows`' new `newContinues` return value**

Add to `internal/terminal/reflow_test.go`:

```go
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
```

- [ ] **Step 7: Run the tests to verify they fail**

Run: `go test ./internal/terminal/... -run TestReflowRowsReturnsContinuationBits -v`
Expected: FAIL — `reflowRows` called with 6 args / 4 return values doesn't match the current 5-arg / 3-return-value signature (compile error).

- [ ] **Step 8: Change `reflowRows`' signature to consume and produce continuation bits**

Replace the existing `reflowRows` function in `internal/terminal/reflow.go` (currently lines 168-190) with:

```go
// reflowRows re-wraps every physical row in rows (at oldWidth) to
// newWidth, and maps (cursorRow, cursorCol) — both 0-indexed physical
// coordinates — into the new layout. This is the single entry point
// Model.SetSize calls; it is stateless (nothing here reads or writes
// any field that persists across calls) — every call re-derives
// entirely from the rows and known continuation bits it's given, which
// is what makes this robust against a shell redrawing or scrolling
// mid-resize (see the design spec's §3.4): there is no earlier-moment
// snapshot for such a redraw to invalidate.
//
// known carries prior-known continuation bits for rows (see
// groupIntoLogicalLinesKnown) — pass nil when there is no trustworthy
// record (fresh content, or after real output invalidated it).
// newContinues is the continuation bits for newRows — the FIRST
// physical row of every rewrapped logical line is false, every row
// after it within that same line is true. The caller (Model.SetSize)
// persists this as the new authoritative record for whatever ends up
// live after height-eviction, so the NEXT resize doesn't have to
// re-guess this same content from its rendered strings (see the
// design spec's "self-tracked continuation bits" section — this is
// the mechanism that stops a single dropped word-boundary space from
// compounding across a long resize sequence).
func reflowRows(rows []string, oldWidth, newWidth, cursorRow, cursorCol int, known []bool) (newRows []string, newCursorRow, newCursorCol int, newContinues []bool) {
	lines, counts := groupIntoLogicalLinesKnown(rows, oldWidth, known)
	cursorLine, offset := cursorOffset(rows, counts, cursorRow, cursorCol)
	for li, line := range lines {
		rewrapped := rewrapLogicalLine(line, newWidth)
		if li == cursorLine {
			r, c := cursorAfterRewrap(rewrapped, offset)
			newCursorRow = len(newRows) + r
			newCursorCol = c
		}
		newRows = append(newRows, rewrapped...)
		for k := range rewrapped {
			newContinues = append(newContinues, k > 0)
		}
	}
	return newRows, newCursorRow, newCursorCol, newContinues
}
```

- [ ] **Step 9: Update the 3 existing `reflowRows` call sites in `reflow_test.go`**

In `internal/terminal/reflow_test.go`:

Change (around line 275):
```go
	newRows, newRow, newCol := reflowRows(rows, 10, 6, 1, 1) // cursor at row1, col1 = the 'b'
```
to:
```go
	newRows, newRow, newCol, _ := reflowRows(rows, 10, 6, 1, 1, nil) // cursor at row1, col1 = the 'b'
```

Change (around line 297):
```go
	newRows, newRow, newCol := reflowRows(rows, 6, 40, 1, 5) // cursor at row1 col5 = 'L'
```
to:
```go
	newRows, newRow, newCol, _ := reflowRows(rows, 6, 40, 1, 5, nil) // cursor at row1 col5 = 'L'
```

Change (around line 308):
```go
	newRows, _, _ := reflowRows(rows, 20, 5, 0, 0)
```
to:
```go
	newRows, _, _, _ := reflowRows(rows, 20, 5, 0, 0, nil)
```

- [ ] **Step 10: Run the full package test suite to verify everything passes**

Run: `go test ./internal/terminal/... -v 2>&1 | tail -60`
Expected: PASS — every test in the package, including the 3 just-updated call sites and all new tests from this task.

- [ ] **Step 11: Commit**

```bash
git add internal/terminal/reflow.go internal/terminal/reflow_test.go
git commit -m "feat: reflowRows consumes and produces wrap-continuation bits"
```

---

### Task 2: Model tracks and uses continuation bits across resizes

**Files:**
- Modify: `internal/terminal/model.go`
- Test: `internal/terminal/model_test.go`

**Interfaces:**
- Consumes: `groupIntoLogicalLinesKnown` (used indirectly through `reflowRows`) and `reflowRows(rows []string, oldWidth, newWidth, cursorRow, cursorCol int, known []bool) (newRows []string, newCursorRow, newCursorCol int, newContinues []bool)` — both from Task 1, already committed.
- Produces: `Model.rowContinues []bool` field; `writeReflowedRows(newRows []string, newCursorRow, newCursorCol, newHeight int, newContinues []bool) Model` (new signature) — nothing outside this package/task calls this, but Task 3's E2E verification exercises the end-to-end behavior it produces.

- [ ] **Step 1: Write the failing regression test for the actual reported bug**

This is the test that must fail against the CURRENT code (before this task's implementation) and pass after. Add to `internal/terminal/model_test.go`:

```go
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
	// because the wrap boundary lands right after a space. If this
	// ever stops being true (e.g. the emulator's rendering changes),
	// this test would otherwise silently stop testing anything.
	oneLine := "  from rendered rows rather than raw terminal cells. Content that scrolls"
	firstRowAt46 := rewrapLogicalLine(oneLine, 46)[0]
	if isRowFilledToEdge(firstRowAt46, 46) {
		t.Fatalf("repro premise broken: row %q unexpectedly measures filled-to-edge at width 46 — this test needs updating, it is no longer exercising the missed-join ambiguity", firstRowAt46)
	}

	widths := []struct{ w, h int }{
		{110, 34}, {102, 33}, {94, 32}, {86, 31}, {78, 30},
		{70, 29}, {62, 28}, {54, 27}, {46, 26}, {38, 25},
		{30, 24}, {28, 23}, {30, 24}, {38, 25}, {46, 26},
		{54, 27}, {62, 28}, {70, 29}, {78, 30}, {86, 31},
		{94, 32}, {102, 33}, {110, 34}, {118, 35},
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/terminal/... -run TestSetSizeSequentialResizesDoNotDropAWordBoundarySpace -v`
Expected: FAIL — either a missing intact phrase, or a glued-word signature found (matching the real bug from the recording).

- [ ] **Step 3: Add the `rowContinues` field to `Model`**

In `internal/terminal/model.go`, add this field to the `Model` struct (after the existing `shrinkOverflow []string` field, inside the struct body ending around line 83):

```go
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
```

- [ ] **Step 4: Thread `rowContinues` through `SetSize`**

In `internal/terminal/model.go`, `SetSize` (currently lines 164-216), change:

```go
		var newRows []string
		var newCursorRow, newCursorCol int
		if doResize {
```
to:
```go
		var newRows []string
		var newCursorRow, newCursorCol int
		var newContinues []bool
		if doResize {
```

Then change:
```go
			if width != m.width {
				newRows, newCursorRow, newCursorCol = reflowRows(oldRows, m.width, width, cy, cx)
			} else {
				// Width didn't change — nothing to rewrap. The old rows and
				// cursor position pass through unchanged; writeReflowedRows
				// below still handles a height change on its own (evicting
				// excess into shrinkOverflow if the new height is smaller).
				newRows, newCursorRow, newCursorCol = oldRows, cy, cx
			}
```
to:
```go
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
```

Then change the final call:
```go
	if doResize {
		m = m.writeReflowedRows(newRows, newCursorRow, newCursorCol, height)
	}
```
to:
```go
	if doResize {
		m = m.writeReflowedRows(newRows, newCursorRow, newCursorCol, height, newContinues)
	}
```

- [ ] **Step 5: Thread `newContinues` through `writeReflowedRows`**

In `internal/terminal/model.go`, change the `writeReflowedRows` signature and body (currently lines 234-291):

```go
func (m Model) writeReflowedRows(newRows []string, newCursorRow, newCursorCol, newHeight int) Model {
	if excess := len(newRows) - newHeight; excess > 0 {
		beforeOverflowLen := len(m.shrinkOverflow)
		m.shrinkOverflow = append(m.shrinkOverflow, newRows[:excess]...)
		if over := len(m.shrinkOverflow) - maxShrinkOverflow; over > 0 {
			m.shrinkOverflow = m.shrinkOverflow[over:]
		}
		if m.scrollOffset > 0 {
			m.scrollOffset += len(m.shrinkOverflow) - beforeOverflowLen
		}
		newRows = newRows[excess:]
		newCursorRow -= excess
	}
```
to:
```go
func (m Model) writeReflowedRows(newRows []string, newCursorRow, newCursorCol, newHeight int, newContinues []bool) Model {
	if excess := len(newRows) - newHeight; excess > 0 {
		beforeOverflowLen := len(m.shrinkOverflow)
		m.shrinkOverflow = append(m.shrinkOverflow, newRows[:excess]...)
		if over := len(m.shrinkOverflow) - maxShrinkOverflow; over > 0 {
			m.shrinkOverflow = m.shrinkOverflow[over:]
		}
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
```

(The `excess <= len(newContinues)` guard covers the pass-through/no-reflow case where `m.rowContinues` was `nil` or shorter than `newRows` — e.g. right after startup, or right after an `OutputMsg` invalidated it — so this can never index out of range; it just degrades to "no tracked record," identical to today's behavior.)

Then, immediately before the existing `m.emu.Write([]byte(buf.String()))` line at the end of the function, add:

```go
	m.rowContinues = append([]bool(nil), newContinues...)
	for len(m.rowContinues) < newHeight {
		m.rowContinues = append(m.rowContinues, false)
	}
```

So the end of the function reads:

```go
	m.rowContinues = append([]bool(nil), newContinues...)
	for len(m.rowContinues) < newHeight {
		m.rowContinues = append(m.rowContinues, false)
	}
	m.emu.Write([]byte(buf.String()))
	return m
}
```

Update the function's doc comment (currently lines 218-233) to add this paragraph at the end:

```go
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
```

- [ ] **Step 6: Invalidate `rowContinues` on real output**

In `internal/terminal/model.go`, `Update`'s `OutputMsg` case (currently around line 400), change:

```go
		m.emu.Write(msg.data)
		switch {
```
to:
```go
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
```

- [ ] **Step 7: Run the regression test to verify it passes**

Run: `go test ./internal/terminal/... -run TestSetSizeSequentialResizesDoNotDropAWordBoundarySpace -v`
Expected: PASS

- [ ] **Step 8: Run the full package test suite**

Run: `go build ./... && go vet ./... && go test ./internal/terminal/... -v 2>&1 | tail -80`
Expected: everything passes, including all of Task 1's tests and every pre-existing test in the package (the `SetSize`/`writeReflowedRows` signature changes are internal to this package — nothing outside it calls either function, so no other package needs updating; confirm this with `grep -rn "writeReflowedRows\|reflowRows(" --include=*.go . | grep -v internal/terminal` returning nothing).

- [ ] **Step 9: Commit**

```bash
git add internal/terminal/model.go internal/terminal/model_test.go
git commit -m "fix: track reflow's own wrap-continuation decisions instead of re-guessing every resize"
```

- [ ] **Step 10: Write the failing test for output-between-resizes invalidation**

Add to `internal/terminal/model_test.go`:

```go
// TestSetSizeRowContinuesInvalidatedByRealOutputBetweenResizes proves
// the safety valve: real output arriving between two resizes must not
// panic or corrupt worse than the pre-existing single-miss heuristic
// behavior — the tracked record simply resets to nil (identical to
// having never reflowed this content before), and the next resize
// falls back to today's heuristic for it, exactly as before this
// feature existed.
func TestSetSizeRowContinuesInvalidatedByRealOutputBetweenResizes(t *testing.T) {
	p := &fakePty{}
	origPty := newPty
	newPty = func(width, height int) (Pty, error) { return p, nil }
	t.Cleanup(func() { newPty = origPty })

	m := New(1).SetSize(40, 10)
	m, _ = m.Start()
	t.Cleanup(func() { _ = m.Close() })

	const line = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123"
	updated, _ := m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte(line)})
	m = updated

	m = m.SetSize(10, 10) // builds a tracked rowContinues record
	if m.rowContinues == nil {
		t.Fatalf("got nil rowContinues after a width-changing resize, want a populated record")
	}

	updated, _ = m.Update(OutputMsg{id: 1, generation: m.generation, data: []byte("more")})
	m = updated
	if m.rowContinues != nil {
		t.Fatalf("got non-nil rowContinues after real output, want nil — output must invalidate the tracked record")
	}

	// Must not panic, and must still produce SOME reasonable output —
	// no crash is the bar here, not perfection.
	m = m.SetSize(40, 10)
	if m.View() == "" {
		t.Fatalf("got empty view after resize following an invalidated record, want non-empty content")
	}
}
```

- [ ] **Step 11: Run the test**

The `OutputMsg` invalidation line this test checks was already added in Step 6 above (needed there to make the main regression test pass), so this is a regression guard confirming that line is correct, not new-behavior TDD.

Run: `go test ./internal/terminal/... -run TestSetSizeRowContinuesInvalidatedByRealOutputBetweenResizes -v`
Expected: PASS. If it fails, Step 6 was not applied correctly — fix `internal/terminal/model.go`'s `OutputMsg` case (the `m.rowContinues = nil` line right after `m.emu.Write(msg.data)`) before continuing.

- [ ] **Step 13: Write the failing test for height-eviction interaction**

Add to `internal/terminal/model_test.go`:

```go
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
```

- [ ] **Step 14: Run the test to verify it fails or passes**

Run: `go test ./internal/terminal/... -run TestSetSizeRowContinuesSurvivesHeightEvictionWithoutPanicking -v`
Expected: PASS if Steps 3-6 were implemented correctly (this test exists to catch an index-out-of-range regression, not to drive new implementation — if it panics, the `excess <= len(newContinues)` guard from Step 5 is missing or wrong; fix `writeReflowedRows` before continuing).

- [ ] **Step 15: Add the missing `"fmt"` import if needed**

Check the top of `internal/terminal/model_test.go` — if `fmt` is not already imported (it likely already is, since other tests in this file use `fmt.Fprintf`; verify with `grep -n '"fmt"' internal/terminal/model_test.go` before assuming either way), add it to the import block.

- [ ] **Step 16: Run the full package test suite one more time**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=3 2>&1 | tail -40`
Expected: all green, three times. (`internal/editor/model.go` may still show up under `gofmt -l .` — pre-existing, unrelated, confirmed in earlier rounds of this same branch's work; do not touch it.)

- [ ] **Step 17: Commit**

```bash
git add internal/terminal/model_test.go
git commit -m "test: cover rowContinues invalidation and height-eviction interaction"
```

---

### Task 3: Real dense corner-drag E2E verification, push

**Files:**
- Modify: `README.md` (only if the verification surfaces something not already documented — otherwise no change needed)

**Interfaces:** none — verification and publishing only. Executed directly by the controller (not delegated to a subagent), matching how the prior plan's own Task 3 was executed, since it requires live judgment calls about tmux capture results.

- [ ] **Step 1: Full suite three times**

```bash
go build ./... && go vet ./... && gofmt -l .
go test ./... -count=3
```

Expected: all green, three times.

- [ ] **Step 2: Real dense corner-drag verification via tmux against an interactive shell**

This must be DENSER than the previous plan's 5-6-jump verification — that thinner density is exactly what let this bug ship undetected. Use 30+ steps.

```bash
go build -o /tmp/cody-continuation-verify ./cmd/cody
tmux kill-session -t contverify 2>/dev/null
tmux new-session -d -s contverify -x 150 -y 45 "/tmp/cody-continuation-verify $(pwd)"
sleep 1
tmux send-keys -t contverify C-t
sleep 1
```

Focus the terminal pane reliably (check after each `Tab` press with an echo probe, not a fixed count):

```bash
for i in 1 2 3; do
  tmux send-keys -t contverify Tab
  sleep 0.4
  tmux send-keys -t contverify -l "echo FOCUSCHK$i"
  tmux send-keys -t contverify Enter
  sleep 0.4
  if tmux capture-pane -t contverify -p | grep -q "FOCUSCHK$i"; then
    echo "focused at tab $i"
    break
  fi
done
tmux send-keys -t contverify -l "cat README.md"
tmux send-keys -t contverify Enter
sleep 1
```

Then run a DENSE corner-drag — write a small script rather than typing 30+ lines inline:

```bash
cat > /tmp/dense_drag.sh << 'SCRIPT'
#!/bin/bash
SIZES_SHRINK="148x44 144x43 140x42 136x41 132x40 128x39 124x38 120x37 116x36 112x35 108x34 104x33 100x32 96x31 92x30 88x29 84x28 80x27 76x26 72x25 68x24 64x23 60x22"
SIZES_GROW="64x23 68x24 72x25 76x26 80x27 84x28 88x29 92x30 96x31 100x32 104x33 108x34 112x35 116x36 120x37 124x38 128x39 132x40 136x41 140x42 144x43 148x44 150x45"
for wh in $SIZES_SHRINK $SIZES_GROW; do
  w="${wh%x*}"
  h="${wh#*x}"
  tmux resize-window -t contverify -x "$w" -y "$h"
  sleep 0.08
done
SCRIPT
chmod +x /tmp/dense_drag.sh
/tmp/dense_drag.sh
sleep 0.5
tmux capture-pane -t contverify -p | tail -15
```

Scroll up through the whole scrollback (SGR mouse wheel-up at coordinates inside the terminal pane's content region — check `tmux capture-pane -t contverify -p | cat -n` first to find the right row/column) and inspect EVERY visible line, not just a sample:

```bash
cat > /tmp/scroll_check.sh << 'SCRIPT'
#!/bin/bash
for i in $(seq 1 25); do
  tmux send-keys -t contverify -l $'\033[<64;100;25M'
done
SCRIPT
chmod +x /tmp/scroll_check.sh
for round in 1 2 3 4; do
  /tmp/scroll_check.sh
  sleep 0.3
  echo "=== scroll round $round ==="
  tmux capture-pane -t contverify -p | cat -n
done
```

Expected: no line anywhere in the visible or scrolled content shows two words fused together with no space, and no README phrase is missing a word. Compare specifically against the failure signatures already seen: a phrase like `terminalcells` (should be `terminal cells`) or any similar fusion. If any is found:
1. First, root-cause whether it is a NEW instance of the fixed bug (this plan's rowContinues tracking has a gap) or the ALREADY-ACCEPTED, pinned single-occurrence heuristic limitation on content's very FIRST reflow (see the design spec's Non-Goals) — check whether the same content, resized through the SAME sequence of widths in isolation as a `go test` case, reproduces it; if it only reproduces on the very FIRST resize step of a never-before-reflowed line, it is the accepted limitation, not a regression.
2. If it is a genuine regression (reproduces across MULTIPLE subsequent resizes after the first, meaning `rowContinues` failed to prevent compounding), STOP — do not proceed to push; the fix is incomplete and needs further investigation, not a workaround.

```bash
tmux kill-session -t contverify 2>/dev/null
rm -f /tmp/cody-continuation-verify /tmp/dense_drag.sh /tmp/scroll_check.sh
```

- [ ] **Step 3: Update the README only if the verification found something new**

If Step 2 passes cleanly with no new observations, skip this step — the existing README wording already documents the single-occurrence heuristic limitation, and this plan's fix is an internal robustness improvement with no new user-visible capability or caveat to document.

- [ ] **Step 4: Push**

```bash
git push origin worktree-terminal-reflow
```

Report the final commit range and confirm PR #9 (already open on this branch) now includes these commits — no new PR needed.
