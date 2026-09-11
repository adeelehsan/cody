# Reflow Wrap-Continuation Self-Tracking — Design

**Status:** approved by user, proceeding straight to implementation plan (no separate spec-review round — the user asked to go from design to a finished PR in one pass).

**Supersedes:** the "out of scope" ruling in `docs/superpowers/specs/2026-09-10-terminal-reflow-design.md` §3.2/§8 that accepted the false-join/missed-join heuristic weaknesses as permanent, untouchable limitations. They are not fully eliminated by this design (see Non-Goals), but their most damaging consequence — compounding across a real multi-step resize drag — is.

## Problem

`docs/superpowers/plans/2026-09-11-unify-resize-reflow.md` fixed corner-drag reflow being skipped outright. After that fix shipped, the user recorded a real Ghostty window drag (`cat README.md`, then drag the corner in and back out) that still destroyed content — down to just the shell prompt.

Root-caused with a deterministic test (`internal/terminal`, real `vt.Emulator`, ~24 sequential `SetSize` calls approximating a smooth drag, no output in between): a real, permanent space character between two words was silently dropped, and by the end of the sequence unrelated words were fused together with no space at all (`terminalcells.Contentthatscrolls`).

Cause: `groupIntoLogicalLines` decides "does row *i* continue the logical line row *i-1* started" using `isRowFilledToEdge` — a heuristic over the row's *rendered string*. The underlying library (`ultraviolet`) initializes every cell, written or not, to the same blank-space value (confirmed by reading `uv.NewBuffer`/`EmptyCell` in the vendored source) — so a row's rendered string cannot distinguish "content that really ends here" from "content that wrapped, and the wrap point happened to land right after a space." When the wrap point does land on a space (very common in ordinary prose), the row's rendered width comes back one short of the pane width, `isRowFilledToEdge` says "no continuation," and the two rows are permanently treated as separate logical lines from then on — the boundary space is gone, not hidden.

That alone is the already-known, already-pinned "missed join" limitation (`TestGroupIntoLogicalLinesMissesAWrapBoundaryEndingInBlank`). What makes it destructive is that **every `SetSize` call re-derives logical lines from scratch, from the same lossy rendered strings** — so a single dropped space at step 3 of a drag can feed a *different* misjudgment at step 11 (an unrelated row's text now coincidentally fills the pane width, gets false-joined with its neighbor), and so on. A real drag fires dozens of `SetSize` calls; the earlier 5-6-jump E2E verification wasn't dense enough to expose the compounding, which is why it passed cleanly and the bug still shipped.

## Non-solutions ruled out

- **Cell-level access (`Emulator.CellAt`).** Already exists on the vendored library, no new API needed — but checked directly against `uv.NewBuffer`/`EmptyCell`: every cell, touched or not, carries `{Content: " ", Width: 1}` by default. There is no bit anywhere in the cell to distinguish "a space was typed here" from "nothing was ever written here." This path is a dead end, not merely undesirable.
- **A looser heuristic** (e.g., "one column short still counts as filled"). Already considered and rejected in the original design (§3.2) — it trades one misjudgment for a more frequent one.
- **Upstreaming a real per-row wrap flag into the library.** The principled long-term fix, and the original spec already earmarked it as the eventual right home for this (§8, "Filing the upstream fix"). Not landable now — third-party maintainers, no control over timeline.

## Design: self-tracked continuation bits

Stop re-deriving continuation from rendered strings for content this code already reflowed once. Remember the true answer instead.

### Data

`Model` gains one field:

```go
// rowContinues holds, for each of the current live rows (same indexing
// as the grid itself, length == m.height when trustworthy), whether
// that row is a soft-wrap continuation of the row above it — as
// determined with certainty by this package's own last reflow, not
// guessed from rendered text. nil means "no trustworthy record for the
// current grid" (nothing has been reflowed yet, or real output arrived
// and invalidated it) — every consumer must treat nil exactly like
// "fall back to today's isRowFilledToEdge heuristic," so this can never
// make behavior worse than before this design, only better.
rowContinues []bool
```

### Production — `reflow.go`

`groupIntoLogicalLines`'s existing 2-argument signature and behavior are preserved unchanged (all 8 existing test call sites keep working with no edits) by making it a thin wrapper:

```go
func groupIntoLogicalLines(rows []string, width int) (lines []string, physicalRowCounts []int) {
	return groupIntoLogicalLinesKnown(rows, width, nil)
}

// groupIntoLogicalLinesKnown is groupIntoLogicalLines with an optional
// authoritative override: when known[i] is available (known != nil and
// i < len(known)), it is trusted outright as "row i continues row i-1"
// instead of being computed from isRowFilledToEdge(rows[i-1], width) —
// this is how reflowRows avoids repeating a rendered-string guess for
// rows it already reflowed once itself (see reflowRows' doc comment).
// A known slice shorter than rows (or nil) falls back to the heuristic
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

`reflowRows` changes signature — it both *consumes* known continuations (to group its input correctly) and *produces* new ones (for its output, so the caller can store them for next time):

```go
// known carries prior-known continuation bits for rows (see
// groupIntoLogicalLinesKnown) — pass nil when there is no trustworthy
// record (fresh content, or after real output invalidated it).
// newContinues is the continuation bits for newRows — the FIRST
// physical row of every rewrapped logical line is false, every row
// after it within that same line is true. The caller (Model.SetSize)
// is expected to persist this as the new authoritative record for
// whatever ends up live after height-eviction.
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

### Consumption and storage — `model.go`

`SetSize`'s capture step passes `m.rowContinues` into `reflowRows` as `known`, and — in the pass-through branch (width unchanged, only height changed) — carries `m.rowContinues` through unchanged as `newContinues` rather than dropping it, since the content didn't move:

```go
if width != m.width {
	newRows, newCursorRow, newCursorCol, newContinues = reflowRows(oldRows, m.width, width, cy, cx, m.rowContinues)
} else {
	newRows, newCursorRow, newCursorCol = oldRows, cy, cx
	newContinues = m.rowContinues
}
```

`writeReflowedRows` gains a `newContinues []bool` parameter, mirrored through its existing eviction logic exactly the way `newRows` already is — the same slice index `excess` that splits `newRows` into "evicted" and "kept" splits `newContinues` identically (evicted entries are simply dropped, scrollback content needs no live tracking), and rows beyond `len(newRows)` (blank padding) get `false`:

```go
func (m Model) writeReflowedRows(newRows []string, newCursorRow, newCursorCol, newHeight int, newContinues []bool) Model {
	if excess := len(newRows) - newHeight; excess > 0 {
		...
		newRows = newRows[excess:]
		newContinues = newContinues[excess:]
		newCursorRow -= excess
	}
	...
	m.rowContinues = append([]bool{}, newContinues...)
	for len(m.rowContinues) < newHeight {
		m.rowContinues = append(m.rowContinues, false)
	}
	return m
}
```

(Exact placement/trimming details are for the implementation plan; the principle is "shift/trim `newContinues` in lockstep with `newRows`, pad the rest false, store the result as the new `m.rowContinues`.")

### Invalidation

Any real output reaching the emulator must invalidate the record — this package cannot safely assume rows it once reflowed are still whatever it left them as once the shell (or an alt-screen app) has written more:

```go
case OutputMsg:
	...
	m.emu.Write(msg.data)
	m.rowContinues = nil
	...
```

This is the safety valve that keeps the design scoped to the actual bug (a corner-drag with nothing being typed mid-drag) instead of attempting the much harder, previously-ruled-out problem of tracking wraps through arbitrary live shell output. The cost: if the user types, then immediately starts dragging, the very next resize's capture falls back fully to the heuristic (identical to today's behavior) — but every resize *after* that first one in the same drag is exact again, because that first reflow rebuilds a fresh, authoritative `rowContinues`.

### Net effect

- Before this design: every resize in a drag re-guesses from scratch → errors compound without bound across a long drag.
- After this design: at most one possible missed-join, on the very first reflow of any given piece of content — every subsequent resize in the same uninterrupted drag is exact, because it's built from what this code itself just wrote, not re-derived from ambiguous rendered text.

## Non-Goals

- This does **not** eliminate the single-occurrence false-join/missed-join heuristic misjudgment on content's *first* reflow — `TestGroupIntoLogicalLinesFalseJoinOnAFullWidthRow` and `TestGroupIntoLogicalLinesMissesAWrapBoundaryEndingInBlank` stay exactly as they are, still pinning that residual, now-non-compounding limitation.
- Does not attempt to track wraps through arbitrary live PTY output (ruled out above) — invalidation on any `OutputMsg` is deliberate, not a gap to close later.
- Does not change `shrinkOverflow`'s own content or eviction direction (already unified in the prior plan) — this only adds a parallel bookkeeping array that shifts alongside it.

## Testing Strategy

- `reflow.go` unit tests: `groupIntoLogicalLinesKnown` — a `known` override wins over a disagreeing heuristic in both directions (forces a join the heuristic wouldn't make; forces a split the heuristic wouldn't make); `known` shorter than `rows` falls back correctly past its end; `nil` known reproduces today's exact behavior (regression guard for the existing 8 call sites' continued correctness).
- `reflowRows` unit test: returned `newContinues` matches the expected split points for a multi-line rewrap.
- `Model`-level regression test (the actual bug): a real `vt.Emulator`, prose content whose wrap boundary is engineered to land exactly on a space at some intermediate width, run through a chained sequence of ~20+ `SetSize` calls with no output in between (mirroring the density of a real drag) — assert the space, and all words, survive intact end to end. This is the test that would have caught this bug before it shipped.
- Invalidation test: an `OutputMsg` between two `SetSize` calls in an otherwise-tracked sequence must not crash or corrupt worse than today's single-miss heuristic behavior (proving the safety valve degrades gracefully, not catastrophically).
- Height-eviction interaction test: a shrink dense enough to evict rows into `shrinkOverflow` while `rowContinues` is populated — assert no index-out-of-range, and that surviving live rows' continuation bits still line up with their (shifted) content.
- Real tmux E2E re-verification with a **much denser** step count than the previous plan's 5-6 jumps (30-50 steps, approximating a real smooth drag) — this is the gap that let the bug ship undetected last time.
