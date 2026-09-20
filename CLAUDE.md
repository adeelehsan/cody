# Cody

A terminal-based code editor written in Go, built on Bubble Tea (Elm architecture: `Model`/`Update`/`View`). See `README.md` for the full feature list and keybindings — don't duplicate that here, keep this file focused on what a coding session needs.

## Build, test, run

Requires a C compiler (CGO) for tree-sitter grammar bindings.

```bash
make build      # go build -o cody ./cmd/cody
make test       # go test ./...
go vet ./...    # CI also runs this (see .github/workflows/ci.yml) — build, vet, test, in that order, no gofmt check
./cody <path>   # open a project; no path defaults to cwd
```

CI does not check `gofmt`. `internal/editor/model.go` is pre-existing `gofmt`-dirty; leave it alone unless you're already editing that file for something else.

## Architecture

`cmd/cody/main.go` wires up the root `tea.Program`. `internal/app` is the top-level `Model` that composes and lays out the other panes and owns focus-cycling (`Tab`/`Shift+Tab`), pane resizing, and the menu bar/command palette. Each pane is its own package with its own `Model`/`Update`/`View`, embedded as a value (not a pointer) into the parent — standard Bubble Tea composition:

- `internal/editor` — the text buffer: editing, undo/redo, selection, tree-sitter-backed syntax highlighting (delegates to `internal/highlight`), code folding, incremental search, split-pane support.
- `internal/filetree` — the project tree sidebar: lazy directory walking, icons, create/rename/delete.
- `internal/terminal` — an embedded shell pane wrapping `charmbracelet/x/vt`'s `Emulator` over a real pty (`charmbracelet/x/xpty`). Supports multiple independent tabs, scrollback, and mouse-wheel scrolling.
- `internal/highlight` — tree-sitter query/highlight glue, shared by `internal/editor`.
- `internal/scrollbar` — shared scrollbar rendering used by both `internal/editor` and `internal/terminal`.
- `internal/statusbar` — the bottom status line.

## The terminal package's reflow mechanism (non-obvious, worth reading before touching `internal/terminal`)

`vt.Emulator`'s own `Resize` has no soft-wrap tracking: a width shrink truncates each row outright, a height shrink just drops rows off the top. `internal/terminal`'s `SetSize`/`reflow.go` compensate by capturing the grid, reflowing it at the new width, and writing the result back before the library's own resize effects would otherwise show through.

The wrap-detection heuristic (`isRowFilledToEdge`: "a row whose rendered width fills the pane is assumed to have soft-wrapped") is fundamentally ambiguous — the library can't distinguish a row that filled by coincidence from one that genuinely wrapped, or a stripped trailing space from one that was never printed. Two consequences, both already load-bearing and covered by tests, not bugs to "fix" casually:

- **Single-occurrence false-join / missed-join** — pinned by `TestGroupIntoLogicalLinesFalseJoinOnAFullWidthRow` / `TestGroupIntoLogicalLinesMissesAWrapBoundaryEndingInBlank`. Accepted, documented in `README.md`'s "Terminal pane limitations" section and in `docs/superpowers/specs/2026-09-10-terminal-reflow-design.md` §3.2/§8.
- **Compounding across a multi-step resize** (e.g. a real window-corner drag) was a real bug, fixed by having the package track its own wrap-continuation decisions (`Model.rowContinues`, `Model.rowWrittenWidths`) instead of re-deriving them from lossy rendered strings on every single resize. See `docs/superpowers/specs/2026-09-11-reflow-continuation-tracking-design.md` for the full story, including a wide-character (CJK) corruption bug the naive version of that fix introduced and how it was caught.
- **That tracking must survive real output, per row** (`Model.rowTracked`, `retrackRowsAfterOutput`). Every pty resize delivers SIGWINCH and an interactive shell answers each one by redrawing its prompt, so an `OutputMsg` lands between *every* pair of `SetSize` calls in a real drag. The first version of the tracking dropped the whole record on any output, which meant it never once engaged against a real shell — while every test (which chained `SetSize` calls with no output in between) passed. See `docs/superpowers/specs/2026-09-20-reflow-tracking-survives-output-design.md`.

If you're debugging a terminal-resize content issue: check whether it's one of the two accepted single-occurrence cases above before assuming it's new. Verify any resize fix against a *dense*, multi-step resize sequence (30+ steps, both dimensions changing every step) — a handful of big jumps is not dense enough to catch compounding, which is exactly how a previous fix round shipped incomplete. A unit-level resize sequence must also inject the shell's SIGWINCH prompt redraw (`"\r\r\x1b[0m\x1b[27m\x1b[24m\x1b[J" + prompt`, as zsh emits it) as an `OutputMsg` after every step — a sequence with no output between resizes models something a real shell never does, which is how the *next* fix round shipped inert. And when checking the tmux E2E result, look for lines left fragmented into short rows after the regrow, not only for fused words.

## Testing conventions

- Table-driven where it fits; otherwise one test function per behavior with a descriptive name that states the expectation, not just the method under test (e.g. `TestReflowRowsKeepsUnrelatedLogicalLinesSeparate`, not `TestReflowRows2`).
- `internal/terminal` tests prefer a real `vt.Emulator` over a hand-rolled fake wherever the behavior depends on the library's actual rendering (trailing-blank stripping, cursor semantics, wide-character width) — a fake emulator can hide exactly the kind of bug this package has been bitten by twice. `fakePty` (not a fake emulator) is the thing that's normally faked.
- For terminal-resize bugs specifically, a unit test alone has not been sufficient evidence of a real fix in this codebase's history — pair it with a real tmux-driven end-to-end check against an actual interactive shell before considering the fix verified. See `docs/superpowers/plans/2026-09-11-reflow-continuation-tracking.md`'s Task 3 for the exact technique (tmux session, SGR mouse-wheel scroll, dense `resize-window` sequence).

## Design history

`docs/superpowers/specs/` and `docs/superpowers/plans/` hold the design docs and implementation plans for past non-trivial features, in date order — read the relevant ones before making structural changes to a package they cover; they carry rationale (including rejected alternatives) that isn't otherwise in the code.

## Finishing a feature

Every completed feature or fix goes through a PR, not a local merge:

1. Push the branch and open a PR against `main` — always take the push+PR path (skip offering the "merge locally / push+PR / keep as-is" choice for this repo).
2. This repo has fewer than 10 GitHub stars, so CodeRabbit does **not**
   auto-review new pushes — trigger it manually after every push (the
   checkbox on its PR comment, or a `@coderabbitai review` comment).
   Greptile does auto-review on push, no manual trigger needed.
3. Wait (~10 min) automatically, without asking first, then check both
   bots' actual current findings — via
   `gh api repos/<owner>/<repo>/pulls/<n>/comments` and the PR's issue
   comments (`gh api repos/<owner>/<repo>/issues/<n>/comments`), not just
   `gh pr view --comments`, which only shows conversation comments and
   misses inline findings. Evaluate each finding against the real,
   current code before reporting it — don't relay bot text verbatim, and
   don't treat a stale review (one that predates the latest push) as
   current.
4. If a finding is a real, valid Critical or Important issue: fix it and
   push a follow-up commit on the same branch without waiting for
   approval first, then repeat step 3 against the new commit. Minor/
   cosmetic findings can just be reported, not auto-fixed.
5. A feature isn't done until this check has completed at least once
   against the final pushed commit with no outstanding valid Critical/
   Important findings.

## Git

PRs target `main`. No branch protection assumptions beyond what CI enforces (vet + build + test).
