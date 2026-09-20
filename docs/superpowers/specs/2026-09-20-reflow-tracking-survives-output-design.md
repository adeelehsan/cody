# Reflow Wrap Tracking That Survives Real Output — Design

**Amends:** `2026-09-11-reflow-continuation-tracking-design.md`, specifically its "Invalidation" section and the Non-Goal "invalidation on any `OutputMsg` is deliberate, not a gap to close later." It was a gap, and it made that whole design inert in real use.

## Problem

After the continuation-tracking fix shipped, a real drag still left content fragmented: shrink the window and drag it back out, and prose that started as full lines stays broken into short rows (`rather than the` / `underlying library`) — the missed-join limitation, compounding across the drag exactly as it did before that fix.

Root cause, confirmed by logging every `SetSize` and `OutputMsg` through a real tmux-driven drag against zsh: **`rowContinues` was `nil` going into every single resize step.** Every `pty.Resize` delivers SIGWINCH, and an interactive shell answers each one by redrawing its prompt. zsh emits, verbatim:

```
\r\r\x1b[0m\x1b[27m\x1b[24m\x1b[J<prompt text>
```

(bash/readline redisplays likewise.) That arrives as an `OutputMsg` between *every* pair of `SetSize` calls, and `Update` dropped the whole record on any output. So the tracking never engaged once outside of tests.

It shipped green because every test that exercised it — including `TestSetSizeSequentialResizesDoNotDropAWordBoundarySpace`, and the tmux E2E's pass criterion ("no fused words") — modeled a resize sequence with nothing arriving in between, or looked for the wrong symptom. With the record always nil, no forced joins ever happen, so nothing fuses; the damage shows up as lines that never *rejoin*, which nobody was checking for.

## Design: invalidate per row, not wholesale

The earlier design was right that this package can't know what arbitrary output did to the grid. But it doesn't need to: it can see, from the outside, which rows the output *didn't* change.

`Model` gains `rowTracked []bool` (same indexing/length/lifecycle as `rowContinues` and `rowWrittenWidths`; all three nil together). `writeReflowedRows` marks every row tracked. `Update`'s `OutputMsg` case, only while a record exists, captures the rendered rows and scrollback length before the write and hands them to `retrackRowsAfterOutput` after it:

- **Scroll shift** = scrollback growth across the write. After-row `i` came from before-row `i + shift`. Rows scrolled in fresh at the bottom have no record.
- **Row `i`'s bit stays tracked iff the row above it is unchanged** (rendered string identical to the row it came from), and it was tracked before. The bit records whether row `i-1` wrapped into row `i` — the same thing a real terminal stores as a "wrapped" flag *on row `i-1`* — so row `i`'s own content changing is irrelevant, just as it is to a real terminal's flag. This matters in practice: a prompt sitting under a full-width row is a known non-continuation, and typing at it must not hand that boundary back to the heuristic, which would false-join the two (caught by `TestUpdateOutputUntracksOnlyTheRowsItChanged` against an earlier "both rows must be unchanged" version of this rule).
- `rowWrittenWidths[i]` is only consulted through a tracked bit `i+1`, which the rule above already ties to row `i` being unchanged — no separate validity tracking needed.
- The record is dropped outright (back to `nil`, and back to zero per-write cost) when nothing worth keeping survives (only boundaries under blank rows), when the scrollback shrank (cleared), or when the alt screen is up. `SetSize` also drops it on a resize it can't reflow (alt screen, zero size).

Anything this can't see through — a scroll region, reverse index, insert/delete line — just makes rows stop matching, which degrades to the heuristic: what every row had before the record existed. A row rewritten with byte-identical content (the SIGWINCH prompt redraw itself) keeps its state, correctly.

**The scrollback cap** (review finding on PR #10). Scrollback growth is only an exact scroll shift until the library's scrollback fills (10,000 lines): past that, each line pushed in evicts one from the far end and growth under-reports — typically reads 0. Rows compared at the wrong offset mostly fail to match, but repeated identical rows match anyway and would inherit an unrelated row's state. The eviction is its own tell: below the cap the *oldest* scrollback line never changes, so `Update` captures `ScrollbackLine(0)` alongside the rows, and a write that changed it drops the record. Non-scrolling writes — the SIGWINCH redraw — are unaffected at any scrollback size, so the fix doesn't quietly switch itself off in long-lived sessions; what a full scrollback costs is only the record surviving *scrolling* output between two drags (back to the accepted single first-reflow guess). Residual: evicted lines textually identical to their successors slip past the check, leaving the bounded heuristic-class misjudgment above, now needing two coincidences. Pinned by `TestUpdateScrollingOutputAtScrollbackCapDropsTheTrackedRecord`, which was first written with distinct rows, passed with the check disabled, and was reworked around repeated rows until it didn't.

`SetSize` now resolves one `known []bool` for the captured rows up front — tracked bit where trusted, `isRowFilledToEdge` otherwise — and uses it for both branches. `reflow.go`'s grouping API is unchanged (it receives a fully-resolved slice).

### Two adjacent bugs fixed along the way

- **Pass-through recorded guesses nobody made.** The width-unchanged branch passed `m.rowContinues` straight through; when that was nil, `writeReflowedRows` stored all-false bits *as certain*, so a genuinely wrapped line on screen during a height-only step (dragging the pane's top edge; a corner-drag step where only height moved) refused to rejoin on the next width change. Previously masked by the wholesale invalidation above. Now the resolved `known` (i.e. the heuristic's answer) is what gets recorded. Pinned by `TestSetSizeHeightOnlyResizeDoesNotRecordFreshWrapsAsCertainNonContinuations`.
- **Cursor slid left over stripped trailing blanks.** `cursorAfterRewrap` clamped a cursor beyond its row's rendered content back to the content's end. The emulator strips trailing blanks on render, so a prompt `"$ "` arrives as `"$"` with the cursor at column 2 — and came out of any resize at column 1, gluing the next typed text onto the prompt (`$ls`). zsh masks this by redrawing its prompt; a program that doesn't (`read -p`, Python's `input()`) wouldn't. `reflowRows` now pads the cursor's line back out to the cursor before rewrapping, so those blanks wrap like the real cells they are. Pinned by `TestReflowRowsKeepsCursorPastStrippedTrailingBlanks`. A first version only kept the leftover columns and clamped to the new width, which a review of PR #10 caught failing at the exact boundary: when the line (with or without a trailing blank) fills the new width exactly, the cursor belongs one past the last column, the clamp parks it *on* the last cell, and the next typed character overwrites it (`123456789l`). That case now gets what a real terminal's pending-wrap state resolves to: an empty continuation row with the cursor at its column 0. Pinned by `TestSetSizeCursorLandingExactlyAtTheNewRightEdgeMovesToAContinuationRow`.

## Cost

While a record exists, each `OutputMsg` renders the grid twice. Measured at ~0.44 ms per message on a 160×50 grid of fully SGR-styled text (vs ~5 µs with no record). Scrolling output ages the record out within one screenful, so sustained cost only applies to non-scrolling updates (a `\r` progress line) under previously-reflowed content — a few percent of a core at 60 updates/s. A session that has never been resized pays nothing. Caching each write's "after" rows as the next write's "before" would halve it; not done, since it adds cross-call state to invalidate for a bounded cost.

## Non-Goals (unchanged)

- The single-occurrence false-join/missed-join on content's *first* reflow stays (both pinned tests untouched).
- `shrinkOverflow`/scrollback content is still never re-wrapped or pulled back into the live grid on a widen. With a short pane this is now the dominant visible effect of a shrink-and-regrow: what stays on screen is restored intact, but what was pushed off stays wrapped narrow in scrollback and the freed rows stay blank.
- zsh's own SIGWINCH redraw assumes its prompt still occupies as many rows as it did at the *old* width; when a prompt that had wrapped is reflowed back onto one row, zsh's cursor-up (`\x1bM`) + `\x1b[J` can erase the content row above it. Every reflowing terminal has this conflict with zsh; not addressed here.

## Verification

- Unit: `TestSetSizeDenseResizeSurvivesShellPromptRedrawAfterEveryStep` (a 46-step dense sequence, both dimensions changing every step, with the verbatim zsh redraw injected after every step; asserts the original lines come back as the same single rows), plus the per-row, scroll-shift, pass-through and cursor tests named above. All fail on the pre-fix code.
- Real tmux E2E against zsh, 46-step corner drag (150×45 → 60×22 → 150×45), terminal pane enlarged to 15 rows, at two step rates. Pre-fix binary: `a (from a height shrink` / `or from` / `reflow` fragments left on screen. Post-fix: every line still on screen restored exactly; a paged sweep of the full scrollback confirmed the whole 17-line passage present, in order, nothing lost or fused.
