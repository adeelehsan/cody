# Unify Width/Height Resize Reflow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close a real gap where a simultaneous width+height resize (a corner-drag — the common way people actually resize windows) skips reflow entirely and hits the original width-truncation bug, by unifying reflow with the pre-existing height-shrink-capture path into one computation that runs on any resize shape.

**Architecture:** `Model.SetSize` runs a single computation for any resize where width and/or height change (still skipped only during `IsAltScreen()`): capture the old grid, reflow it at the new width if width changed, then hand the result to the *already-shipped, unchanged* `writeReflowedRows` (which already implements "evict oldest to `shrinkOverflow`, keep newest live"). The old height-shrink-only capture block and the `shrinkContinuing` gesture-tracking flag are deleted entirely — reflow's per-call statelessness already produces correct order without them.

**Tech Stack:** Go, existing `internal/terminal` package — no new dependencies, no `Emulator` interface changes.

**Spec:** `docs/superpowers/specs/2026-09-10-terminal-reflow-design.md` (see the 2026-09-11 addendum and the rewritten §5)

## Global Constraints

- `reflowRows`, `rewrapLogicalLine`, `groupIntoLogicalLines`, `cursorOffset`, `cursorAfterRewrap` (all of `reflow.go`) are **not modified** by this plan — they're already correct for the new unified direction.
- `writeReflowedRows` (in `model.go`) is **not modified** by this plan — it already implements "evict oldest `[:excess]` into `shrinkOverflow`, keep newest `[excess:]` live," which is exactly the direction this plan unifies on.
- TDD: write the failing test, watch it fail, implement, watch it pass.
- Every fix must be verified against the **real** `vt.Emulator`, not just the fake, for anything touching actual resize/reflow behavior.
- `gofmt`-clean; `go vet ./...` and the full `go test ./...` suite green before every commit.
- This plan removes the `shrinkContinuing` field entirely, along with both of its reset triggers (on output, on height growth) and the prepend-vs-append branch in the old height-shrink capture code.
- The real end-to-end verification for this plan is a simulated **corner-drag** (width and height changing together on every intermediate step) — this is the exact scenario that exposed the bug, not a single-edge width-only drag (already covered by the prior plan).

---

### Task 1: Unify SetSize into one resize computation; update existing tests to the corrected direction

**Files:**
- Modify: `internal/terminal/model.go`
- Modify: `internal/terminal/model_test.go`

**Interfaces:**
- Consumes: `reflowRows` (unchanged, from `reflow.go`), `Model.writeReflowedRows` (unchanged)
- Removes: `Model.shrinkContinuing` field and every reference to it

- [ ] **Step 1: Replace `SetSize`'s body**

In `internal/terminal/model.go`, replace the entire `SetSize` function body (from `func (m Model) SetSize(width, height int) Model {` through its closing `}`) with:

```go
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
		if width != m.width {
			newRows, newCursorRow, newCursorCol = reflowRows(oldRows, m.width, width, cy, cx)
		} else {
			// Width didn't change — nothing to rewrap. The old rows and
			// cursor position pass through unchanged; writeReflowedRows
			// below still handles a height change on its own (evicting
			// excess into shrinkOverflow if the new height is smaller).
			newRows, newCursorRow, newCursorCol = oldRows, cy, cx
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
		m = m.writeReflowedRows(newRows, newCursorRow, newCursorCol, height)
	}
	return m
}
```

- [ ] **Step 2: Replace `SetSize`'s doc comment**

Replace the entire doc comment block above `SetSize` (everything from `// SetSize resizes the pane.` through the line right before `func (m Model) SetSize`) with:

```go
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
```

- [ ] **Step 3: Remove the `shrinkContinuing` field**

In the `Model` struct, delete the `shrinkContinuing bool` field and its entire doc comment (the block starting `// shrinkContinuing is true immediately after a shrink-capture...` through the line just before the struct's closing `}`).

- [ ] **Step 4: Update `shrinkOverflow`'s doc comment**

Replace `shrinkOverflow`'s doc comment with:

```go
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
```

- [ ] **Step 5: Search for and remove every remaining reference**

Run `grep -rn "shrinkContinuing" internal/terminal/` — it must return nothing once this task is done. Any remaining hit is a leftover reference to remove.

- [ ] **Step 6: Update the 4 existing tests whose expected values flip with the corrected direction**

The direction is now "keep the newest rows live, evict the oldest to `shrinkOverflow`" (previously, a pure height shrink kept the oldest/top live and evicted the newest/bottom — a workaround for a library constraint that no longer applies). Update these four tests in `internal/terminal/model_test.go`:

**`TestSetSizeCapturesRowsAHeightShrinkWouldOtherwiseDiscard`** — replace the body from `want := []string{...}` through the end of the function with:

```go
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
```

Also update the comment above `want := ...` (currently explaining the OLD "library keeps Lines[:2]... so the rows that actually need capturing are line2/line3/line4" reasoning) to instead explain the NEW direction: the unified computation always evicts the oldest rows and keeps the newest live, independent of whatever the library's own resize does to cell content, since `writeReflowedRows` overwrites it afterward regardless.

**`TestSetSizeTrimsTrailingBlankPaddingFromShrinkCapture`** — replace the body from `want := ...` through the end with:

```go
	// oldRows trims down to ["line0","line1","line2"] (cursor is on
	// row2; rows 3-4 are blank padding). Shrinking 3 rows to height 2
	// evicts the oldest 1 ("line0"), keeping the newest 2 ("line1",
	// "line2") live.
	want := []string{"line0"}
	if len(m.shrinkOverflow) != len(want) || m.shrinkOverflow[0] != want[0] {
		t.Fatalf("got shrinkOverflow=%q, want %q (blank padding trimmed before eviction, and only the oldest meaningful row evicted)", m.shrinkOverflow, want)
	}
```

**`TestSetSizePreservesRowsLostToARealEmulatorsHeightShrink`** — replace the body from `m = m.SetSize(20, 2)` (the shrink call) through the end of the function with:

```go
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
```

Also update the test's own doc comment (the "// The pane only shows 2 rows..." block and the bug-report framing) to describe the corrected direction rather than the old top-stays-live one.

**`TestShrinkOverflowRendersBetweenRealScrollbackAndLiveNotBeforeScrollback`** — replace the body from `m = m.SetSize(20, 2)` (the shrink call) through the end of the function with:

```go
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
```

Update the test's own doc comment similarly (it currently says "captures mid2, mid3, mid4" — now it's "evicts mid0, mid1, mid2, keeping mid3, mid4 live").

- [ ] **Step 7: Delete the two `shrinkContinuing`-specific tests**

Delete these two complete test functions from `internal/terminal/model_test.go` (they test a mechanism that no longer exists) — from each one's `func Test...` through its closing `}`, including its doc comment:

- `TestShrinkContinuingResetsAfterOutputSoALaterShrinkAppends`
- `TestShrinkContinuingResetsOnGrowSoABounceDoesNotMisorderShrinkOverflow`

- [ ] **Step 8: Delete the obsolete simultaneous-change-skips-reflow test**

Delete `TestSetSizeSkipsReflowOnSimultaneousWidthAndHeightChange` in full — its premise (reflow is skipped when both dimensions change) is exactly what this plan removes. Task 2 replaces it with a test asserting the corrected behavior.

- [ ] **Step 9: Confirm the direction-agnostic tests still pass unchanged**

`TestSequentialSmallShrinksProduceTheSameOrderAsOneBigShrink`, `TestSetSizeShrinkCaptureIsSkippedDuringAltScreen`, `TestSetSizeShrinkCaptureIsCappedLikeTheRealScrollbackBuffer`, and `TestSetSizeDoesNotCaptureOnAWidthOnlyShrink` never hardcode which specific rows end up where — they check internal self-consistency (stepwise vs. one-jump equality), a count, or emptiness. They should pass with NO changes needed. Run them explicitly to confirm:

```bash
go test ./internal/terminal/... -run 'TestSequentialSmallShrinksProduceTheSameOrderAsOneBigShrink|TestSetSizeShrinkCaptureIsSkippedDuringAltScreen|TestSetSizeShrinkCaptureIsCappedLikeTheRealScrollbackBuffer|TestSetSizeDoesNotCaptureOnAWidthOnlyShrink' -v
```

If any of these fail unexpectedly, stop and report — that means the hand-derived reasoning above (that these are direction-agnostic) was wrong for that specific test, and it needs its own fix, not a forced pass.

- [ ] **Step 10: Confirm the already-shipped width-reflow tests still pass unchanged**

These were already built and reviewed assuming the "evict oldest, keep newest" direction — they should need no changes at all: `TestSetSizeReflowsWidthShrinkWithoutLoss`, `TestSetSizeReflowsWidthGrowRejoinsWrappedContent`, `TestSetSizeReflowRespondsToOutputArrivingMidResize`, `TestSetSizeReflowLeavesNoStaleTailWhenARowGetsShorter`, `TestSetSizeReflowShrinkThenGrowLeavesNoDuplicateRows`, `TestSetSizeReflowOverflowAppendsIntoShrinkOverflow`, `TestSetSizeReflowOverflowPinsAPausedViewport`, `TestSetSizeReflowDoesNotReflowContentAlreadyInShrinkOverflow`, `TestSetSizeReflowSkippedDuringAltScreen`. Run the full package to confirm:

```bash
go test ./internal/terminal/... -count=1 -v 2>&1 | tail -80
```

- [ ] **Step 11: Full package check and commit**

```bash
go build ./... && go vet ./... && gofmt -l internal/terminal
go test ./... -count=1
git add internal/terminal/model.go internal/terminal/model_test.go
git commit -m "feat: unify width and height resize handling into one reflow computation

Removes shrinkContinuing and the old height-only capture path entirely, replacing both with the same stateless computation reflow already used for width-only resizes — now covering height-only and simultaneous width+height resizes too, and correcting a direction inconsistency between the two prior mechanisms (see design spec's 2026-09-11 addendum)."
```

---

### Task 2: Prove the actual bug is fixed — simultaneous resize tests

**Files:**
- Modify: `internal/terminal/model_test.go`

**Interfaces:** none new — exercises `Model.SetSize` exactly as external callers do.

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run tests to verify they fail against the PRE-Task-1 code**

This step only applies if Task 1 hasn't been committed yet when this task runs — if Task 1 is already committed, skip straight to Step 3 and note in your report that you could not demonstrate the pre-fix failure directly; the fact that Task 1's own tests (from its Step 6-8) already prove the old skip-on-simultaneous-change behavior existed is sufficient prior evidence. If Task 1 is NOT yet committed, run:

```bash
go test ./internal/terminal/... -run 'TestSetSizeReflowsWidthOnASimultaneousWidthAndHeightChange|TestSetSizeSimulatedCornerDragPreservesContentBothWays' -v
```

Expected: FAIL, with truncated content (e.g. `ABCDEFGHIJ` present but the line's tail missing) — reflow was skipped for these calls since both dimensions changed together.

- [ ] **Step 3: Run tests to verify they pass**

```bash
go test ./internal/terminal/... -run 'TestSetSizeReflowsWidthOnASimultaneousWidthAndHeightChange|TestSetSizeSimulatedCornerDragPreservesContentBothWays' -v
```

Expected: PASS (both).

- [ ] **Step 4: Full package check and commit**

```bash
go build ./... && go vet ./... && gofmt -l internal/terminal
go test ./... -count=1
git add internal/terminal/model_test.go
git commit -m "test: prove width is protected on a simultaneous width+height resize"
```

---

### Task 3: Real corner-drag E2E verification, docs, push

**Files:**
- Modify: `README.md` (only if the corner-drag verification surfaces something not already documented — otherwise no change needed)

**Interfaces:** none — verification and publishing only.

- [ ] **Step 1: Full suite three times**

```bash
go build ./... && go vet ./... && gofmt -l .
go test ./... -count=3
```

Expected: all green, three times. (`internal/editor/model.go` may still show up under `gofmt -l .` — pre-existing, unrelated, confirmed in earlier rounds of this same branch's work; do not touch it.)

- [ ] **Step 2: Real corner-drag verification via tmux against an interactive shell**

This is the actual bar this plan has to clear — the exact reproduction that exposed the bug.

```bash
go build -o /tmp/cody-unify-verify ./cmd/cody
tmux kill-session -t unifyverify 2>/dev/null
tmux new-session -d -s unifyverify -x 150 -y 45 "/tmp/cody-unify-verify $(pwd)"
sleep 1
tmux send-keys -t unifyverify C-t
sleep 1
```

Focus the terminal pane reliably (check after each `Tab` press with an echo probe, not a fixed count):

```bash
for i in 1 2 3; do
  tmux send-keys -t unifyverify Tab
  sleep 0.4
  tmux send-keys -t unifyverify -l "echo FOCUSCHK$i"
  tmux send-keys -t unifyverify Enter
  sleep 0.4
  if tmux capture-pane -t unifyverify -p | grep -q "FOCUSCHK$i"; then
    echo "focused at tab $i"
    break
  fi
done
tmux send-keys -t unifyverify -l "cat README.md"
tmux send-keys -t unifyverify Enter
sleep 1
```

Then run a **corner-drag** (both dimensions changing on every step — this is the part the prior plan's E2E verification never tested):

```bash
tmux resize-window -t unifyverify -x 130 -y 40
sleep 0.15
tmux resize-window -t unifyverify -x 110 -y 36
sleep 0.15
tmux resize-window -t unifyverify -x 90 -y 32
sleep 0.15
tmux resize-window -t unifyverify -x 70 -y 28
sleep 0.15
tmux resize-window -t unifyverify -x 60 -y 25
sleep 0.5
tmux resize-window -t unifyverify -x 70 -y 28
sleep 0.15
tmux resize-window -t unifyverify -x 90 -y 32
sleep 0.15
tmux resize-window -t unifyverify -x 110 -y 36
sleep 0.15
tmux resize-window -t unifyverify -x 130 -y 40
sleep 0.15
tmux resize-window -t unifyverify -x 150 -y 45
sleep 0.5
tmux capture-pane -t unifyverify -p | tail -15
```

Scroll up (SGR mouse wheel-up at coordinates inside the terminal pane's content region — check `tmux capture-pane -t unifyverify -p | cat -n` first to find the right row/column for the pane's content area, since this shifts between runs) to inspect further back:

```bash
for i in $(seq 1 20); do
  tmux send-keys -t unifyverify -l $'\033[<64;100;25M'
done
tmux capture-pane -t unifyverify -p | tail -15
```

Expected: no line anywhere in the visible or scrolled content is cut off mid-word (compare against the exact pattern from the original bug report and this plan's own reproduction — e.g. `completion.zsh.inc` must never appear as `completion.zsh.i`). If any truncation is found, STOP — do not proceed to push; the fix is incomplete and needs further investigation, not a workaround.

```bash
tmux kill-session -t unifyverify 2>/dev/null
rm -f /tmp/cody-unify-verify
```

- [ ] **Step 3: Update the README only if the verification found something new**

If Step 2 passes cleanly with no new observations, skip this step — the existing README wording (reflow re-wraps on width change, documented heuristic limitations) already covers the corrected behavior; nothing about the corner-drag fix needs its own README bullet, since from the user's perspective it's simply "resizing now works," not a new capability with its own caveat.

- [ ] **Step 4: Push**

```bash
git push origin worktree-terminal-reflow
```

Report the final commit range and confirm PR #9 (already open on this branch) now includes these commits — no new PR needed.
