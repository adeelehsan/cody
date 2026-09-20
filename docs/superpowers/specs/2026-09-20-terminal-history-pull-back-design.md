# Terminal History Ownership and Pull-Back on Regrow — Design

**Supersedes:** `2026-09-10-terminal-reflow-design.md` §8's ruling that content pushed into the resize overflow "is never re-reflowed by a later resize" (pinned until now by `TestSetSizeReflowDoesNotReflowContentAlreadyInShrinkOverflow`), and the README limitation that documented it. Builds on `2026-09-20-reflow-tracking-survives-output-design.md`.

## Problem (user-reported)

`cat README.md` in the terminal pane, shrink the window as far as it goes, restore it: the content is gone and never comes back — just the prompt in an otherwise empty pane.

Reproduced with a tmux-driven run against zsh (150x45 → 2x2 → 150x45). Two independent causes:

1. **`internal/app`: a window shrink permanently shrank the panes.** Every `WindowSizeMsg` re-clamped the previous *effective* tree width / terminal height / split column, so a clamp forced by a tiny window became the new size for good: the terminal pane came back 1 row tall (tree 15 columns wide). Even a perfect reflow can't show content in a one-row pane.
2. **`internal/terminal`: rows a resize pushed out were never given back.** They sat in `shrinkOverflow`, wrapped at whatever few columns the pane had passed through (mixed with 3-column fragments of zsh's redrawn prompt), and no later resize ever touched them. Worse, a zero-size pane reached the emulator: rows "written back" into a zero-width grid are destroyed outright, and the next resize skipped reflow entirely because its *old* size was zero.

## Design

### App: preferences vs. effective sizes

`Model` keeps `prefTreeWidth` / `prefTerminalHeight` / `prefSplitCol` — what the user last chose (defaults, or wherever a drag left it) — separately from the effective `treeWidth` / `terminalHeight` / `splitCol`. `WindowSizeMsg` derives effective = clamp(preference); only a drag (or creating the split) writes a preference.

### Terminal: one history, owned by the package

The old model had three tiers — emulator scrollback, `shrinkOverflow`, live grid — rendered in that order. That order is wrong whenever output scrolls off *after* a shrink (it's newer than the overflow but rendered before it; documented at the time as an accepted edge case), and that is exactly what happens in the reported scenario: at tiny sizes the shell's wrapped prompt redraw scrolls on every step. Rows can only be given *back* from a single correctly-ordered list, so:

- **`Model.history`** (renamed from `shrinkOverflow`) is everything above the live grid, oldest first, with parallel `historyMeta` (`width` the row was laid out at; `continues`/`known` wrap bit). It is fed by resize eviction (`writeReflowedRows`, exact bits) and by **draining the emulator's scrollback after every write** (`absorbOutput`; new `Emulator.ClearScrollback`). The emulator's scrollback is therefore always empty between writes. `renderScrolledView` still reads it first, for completeness only.
- Rows that scroll off carry their tracked state into history: known bit, and the trailing blank the emulator's render stripped restored from `rowWrittenWidths` — the same padding `SetSize` does for live rows. (Without it, a tracked wrapped line that scrolls off mid-drag comes back with the words either side of the wrap fused.) Draining also makes the row alignment in `absorbOutput` trivial — rows scrolled off by this write + the live grid after it = the grid before it, index for index — and removes the scrollback-cap ambiguity the previous design had to detect, since the library's scrollback never fills.
- **`Model.pullable`**: how many trailing history rows are owed back. Resize eviction adds to it; rows drained while it is non-zero add to it too (they sit between the owed rows and the live grid, so they have to come along — this is what keeps the regrow in order). Every `SetSize` takes that tail (`pullableTail`: capped at `maxReflowTail` = 4000 rows, ~2 ms measured; a height-only change takes at most two screenfuls since same-width rows re-wrap to themselves; extended back to a logical-line start), removes it from history, reflows tail + live rows as one run, and `writeReflowedRows` keeps the newest `height` rows live and pushes the rest back out.
- **Ordinary scrollback is never pullable.** With `pullable == 0`, a width change leaves history alone, as before. And a clear-screen in the output (`ESC[2J`, or `ESC[H ESC[J`) zeroes `pullable`: after `clear`, no resize drags old output back in front of the user. `ESC[3J` clears history outright — the library would have cleared its scrollback, which now lives here.
- **Emulator size floor.** The emulator/pty are never sized below 1x1 (`minEmuWidth`/`minEmuHeight`). `Model.width/height` stay what the caller requested (so the app's per-frame `SetSize` stays idempotent); `emuW/emuH` are what reflow arithmetic uses; `View` returns `""` while the requested size has a zero dimension. This removes the zero-size special cases instead of handling them: there is always a live row for the cursor.

`reflow.go` is unchanged by this design.

## Behavior changes to be aware of

- Growing the pane's height now pulls rows back from history whenever earlier resizes pushed rows out (previously: blank rows at the bottom).
- `Start()` on a never-sized model followed by `SetSize(80, 24)` no longer issues a redundant pty resize (the pty is already 80x24).
- Scrollback that scrolled off after a resize eviction is re-wrapped on width changes (up to `maxReflowTail` rows). Deep scrollback beyond that, and all scrollback in a session where no resize ever pushed rows out, still stays at the width it scrolled off at.

## Not addressed

- Resizing while the alt screen is up still skips reflow; the library truncates the main screen underneath. Pre-existing; now listed in the README.
- zsh's SIGWINCH redraw at widths narrower than its prompt can leave prompt fragments in history (every reflowing terminal has this conflict). In the verified scenario none survived to the restored view.

## Verification

- Unit (all failing before, passing after): `TestSetSizeShrinkingToNothingAndBackBringsTheContentBack` (down to 0x0 and back), `TestSetSizeDenseShrinkAndRegrowWithPromptRedrawsBringsEvictedRowsBack` (70+ steps, both dimensions, verbatim zsh redraw after each), `TestSetSizePullBackKeepsOutputThatScrolledWhileSmallInOrder`, `TestSetSizeDoesNotRefillAClearedScreenFromOrdinaryScrollback`, `TestUpdateClearScreenCancelsWhatResizesOweBack`, `TestUpdateEraseScrollbackSequenceClearsTheHistory`, `TestUpdateRowsScrollingIntoHistoryKeepTheirWrapStateAndTrailingBlank`, `TestUpdateDrainsTheEmulatorScrollbackIntoHistoryAfterEveryWrite`, `TestViewNeverExceedsARequestedSizeBelowTheEmulatorFloor`, `TestSetSizeReflowRewrapsRowsAnEarlierResizePushedOut` (replaces the test that pinned the old limitation); app: `TestWindowShrinkDoesNotPermanentlyShrinkThePanes`, `TestWindowShrinkKeepsADraggedPaneSizeAsThePreference`.
- Real tmux E2E against zsh: `cat README.md`, 150x45 → 120x35 → … → 2x2 → … → 150x45: pane content, prompt and layout byte-identical before/after; 120 lines of scrollback above it clean and full-width. The 46-step corner drag from the previous design (terminal pane enlarged to 15 rows) is now byte-identical before/after at two step rates — previously it restored only the lines that had stayed on screen. `seq 1 300000` throughput unchanged vs. `main` (2.1 s vs 3.2 s).
