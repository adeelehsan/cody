package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"cody/internal/editor"
	"cody/internal/filetree"
	"cody/internal/highlight"
	"cody/internal/terminal"
)

func mustWriteAndReturn(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func keyRune(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func TestTabTogglesFocus(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.focus != focusTree {
		t.Fatal("expected initial focus on tree")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.focus != focusEditor {
		t.Fatal("expected focus to move to editor")
	}
}

func TestFileOpenedMsgLoadsEditorAndSwitchesFocus(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)
	if m.focus != focusEditor {
		t.Fatal("expected focus to move to editor")
	}
	if !m.activeEditor().HasBuffer() {
		t.Fatal("expected editor to have a loaded buffer")
	}
}

func TestCtrlSWorksRegardlessOfFocus(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)
	// Focus is now on the editor after opening; switch back to the tree.
	// Shift+Tab (not Tab) goes editor -> tree in the three-way focus cycle
	// (Tree -> Editor -> Terminal -> Tree forward, so backward from Editor
	// lands on Tree).
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = updated.(Model)
	if m.focus != focusTree {
		t.Fatal("expected focus back on tree")
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("expected ctrl+s to produce a save command even with the tree focused")
	}
	msg, ok := cmd().(editor.CommandExecutedMsg)
	if !ok {
		t.Fatalf("got %T, want editor.CommandExecutedMsg", msg)
	}
}

func TestCtrlQQuits(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlQ})
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("expected the command to produce tea.QuitMsg")
	}
}

func TestFullFlowOpenTypeSaveUpdatesStatusBar(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	tree, cmd := m.tree.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.tree = tree
	if cmd == nil {
		t.Fatal("expected a command opening a.go")
	}

	updated, _ = m.Update(cmd())
	m = updated.(Model)
	if m.focus != focusEditor || !m.activeEditor().HasBuffer() {
		t.Fatal("expected file opened and focus moved to the editor")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	m = updated.(Model)

	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("expected ctrl+s to produce a save command")
	}
	updated, _ = m.Update(cmd())
	m = updated.(Model)

	if m.recentCommand != "Saved a.go" {
		t.Fatalf("got recentCommand=%q, want %q", m.recentCommand, "Saved a.go")
	}
}

func TestMouseClickFileOpenThenTypeThenLoadsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target.go"), []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	// Click "File" (columns 0-3, row 0).
	updated, _ = m.Update(tea.MouseMsg{X: 0, Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)
	if m.openMenu != "File" {
		t.Fatalf("got openMenu=%q, want File", m.openMenu)
	}

	// Click "Open" (second item in the File dropdown, after "New"; y=3 skips
	// the menu bar row, the dropdown's own top border row, and "New"'s row).
	updated, _ = m.Update(tea.MouseMsg{X: 0, Y: 3, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)
	if m.activeDialog != dialogFileOpen {
		t.Fatal("expected clicking Open to activate the file-open dialog")
	}

	// Type the path and confirm.
	for _, r := range "target.go" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.activeDialog != dialogNone {
		t.Fatal("expected the dialog to close after a successful open")
	}
	if !m.activeEditor().HasBuffer() {
		t.Fatal("expected the editor to have loaded target.go")
	}
	if m.focus != focusEditor {
		t.Fatal("expected focus on the editor")
	}
}

func TestMouseClickCommandsThenFilterThenEnterRunsSave(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "target.go")
	if err := os.WriteFile(file, []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	// Click "Commands" (columns 12-19, row 0 — see menuLabels()).
	updated, _ = m.Update(tea.MouseMsg{X: 12, Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)
	if m.activeDialog != dialogPalette {
		t.Fatalf("got activeDialog=%v, want dialogPalette", m.activeDialog)
	}

	// Type "save" to filter down to a single match.
	for _, r := range "save" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	matches := filteredCommands(m.commands, m.paletteFilter.Value())
	if len(matches) != 1 || matches[0].Name != "Save" {
		t.Fatalf("got matches=%v", matches)
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	if m.activeDialog != dialogNone {
		t.Fatal("expected the palette to close after running the selected command")
	}
	// Running a command from the palette (like pressing its shortcut key
	// directly) returns a tea.Cmd rather than updating the status bar
	// synchronously — cmdSave's own tea.Cmd produces an
	// editor.CommandExecutedMsg that must flow back through Model.Update
	// for m.recentCommand to be set, exactly as the real Bubble Tea
	// runtime loop would do it. Without this step, m.recentCommand would
	// still hold the stale "Opened target.go" message set by the earlier
	// FileOpenedMsg handler, and the test would pass even if Save's
	// result never reached the status bar at all.
	if cmd == nil {
		t.Fatal("expected running Save from the palette to return a command")
	}
	updated, _ = m.Update(cmd())
	m = updated.(Model)
	if m.recentCommand != "Saved target.go" {
		t.Fatalf("got recentCommand=%q, want %q", m.recentCommand, "Saved target.go")
	}
}

func TestWindowSizeUpdatesEvenWithDialogOpen(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	m, _ = m.clickMenuLabel("About")
	if m.activeDialog != dialogAbout {
		t.Fatal("setup failed, expected the About dialog to be open")
	}

	updated, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(Model)
	if m.width != 120 || m.height != 40 {
		t.Fatalf("got width=%d height=%d, want 120x40 (resize must apply even with a dialog open)", m.width, m.height)
	}
}

func TestViewRendersAtSmallSize(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	m = updated.(Model)
	if view := m.View(); view == "" {
		t.Fatal("expected a non-empty rendered view at a small terminal size")
	}
}

// TestRehighlightMsgReachesEditorEvenWhenTreeIsFocused is a regression test
// for a bug where app.Model.Update routed any unrecognized message type to
// whichever component currently had focus. Since the debounced rehighlight
// tick wasn't specifically handled, switching focus to the tree within the
// 150ms debounce window caused the tick to be silently delivered to the
// tree instead of the editor, leaving syntax highlighting stale.
//
// editor.RehighlightMsg's generation field is unexported, so this test
// can't construct one with a matching generation from outside the editor
// package. Instead it drives the real pipeline end to end: it captures the
// actual tea.Cmd returned by typing (the real scheduleRehighlight tick,
// unmodified), switches focus to the tree, then invokes that command for
// real — which really sleeps out the 150ms debounce window — and feeds the
// resulting real RehighlightMsg through app.Model.Update while the tree has
// focus. It then inspects the *rendered ANSI styling* (forcing TrueColor,
// since the default test color profile strips all styling) to prove the
// editor actually reparsed: corrupting "package" into "Xpackage" removes
// the tree-sitter keyword match, so if the tick reached the editor, the
// stale span (which would otherwise mis-highlight "Xpackag" as a keyword,
// misaligned by the inserted character) must be gone.
func TestRehighlightMsgReachesEditorEvenWhenTreeIsFocused(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(termenv.Ascii)

	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	keywordStyle, ok := highlight.StyleFor("keyword")
	if !ok {
		t.Fatal("setup failed: expected a registered keyword style")
	}
	staleStyledText := keywordStyle.Render("Xpackag")

	// Prepend "X" to the first line via a real keypress through the
	// composed app's Update, capturing the real scheduled tea.Cmd.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("setup failed: expected typing to schedule a rehighlight command")
	}

	// Switch focus to the tree before the debounce tick would normally
	// fire — this is exactly the scenario the bug report describes.
	// Shift+Tab (not Tab) goes editor -> tree in the three-way focus cycle
	// (Tree -> Editor -> Terminal -> Tree forward, so backward from Editor
	// lands on Tree).
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = updated.(Model)
	if m.focus != focusTree {
		t.Fatal("setup failed: expected focus on the tree")
	}

	// Invoke the actual scheduled command for real. This really sleeps out
	// the 150ms debounce window and returns the genuine editor.RehighlightMsg
	// (with its unexported generation field set correctly by editor code),
	// rather than fabricating one with a guessed generation value.
	msg := cmd()
	updated, _ = m.Update(msg)
	m = updated.(Model)

	view := m.activeEditor().View()
	if strings.Contains(view, staleStyledText) {
		t.Fatal("expected the rehighlight tick to reach the editor and reparse even though the tree was focused, but the stale (pre-edit) keyword span was still applied to the post-edit text")
	}
}

func TestOpeningAGoFileHighlightsItInTheComposedApp(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	if !m.activeEditor().HasBuffer() {
		t.Fatal("expected the file to be loaded")
	}
	view := m.View()
	if !strings.Contains(view, "func") {
		t.Fatalf("expected the rendered view to contain the source text, got %q", view)
	}
}

func TestFoldingAGoFunctionThroughTheComposedApp(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	src := "package main\n\nfunc add(a int, b int) int {\n\treturn a + 42\n}\n\nvar x = 1\n"
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	for i := 0; i < 2; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	m = updated.(Model)

	view := m.View()
	if strings.Contains(view, "return a + 42") {
		t.Fatal("expected the folded function body to be hidden from the rendered view")
	}
	if !strings.Contains(view, "var x = 1") {
		t.Fatal("expected content after the fold to still render")
	}
}

func TestLargeFileDoesNotPushTheTreePaneOutOfViewAndClipsToWindowHeight(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "biglist.txt")
	lines := make([]string, 500)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i)
	}
	src := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	for i := 0; i < 400; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = updated.(Model)
	}

	view := m.View()
	if !strings.Contains(view, "biglist.txt") {
		t.Fatal("expected the tree pane (showing biglist.txt) to remain visible alongside a large open file")
	}
	if got := strings.Count(view, "\n"); got > 24 {
		t.Fatalf("got %d rendered lines, want at most the window height (24) — content must not overflow the terminal", got)
	}
}

// Regression test: a file whose lines are wider than the editor pane must
// not push the tree pane out of view. app/model.go composes each pane by
// applying a Width()+Height() style to the pane's ALREADY-RENDERED
// multi-line block (editorStyle.Render(m.activeEditor().View())) — if any single
// line inside that block is wider than the style's target width, Lip Gloss
// hard-wraps it into multiple physical lines, and since Height() only sets
// a minimum (never truncates), those extra wrapped lines silently overflow
// the pane's box just like the original "too many lines" bug, except
// triggered by line WIDTH instead of line COUNT.
func TestWideLinesDoNotPushTheTreePaneOutOfViewEither(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "wide.go")
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString(fmt.Sprintf("\tresult := someFunction(argumentOne, argumentTwo, argumentThree, argumentFour) // line %d with a trailing comment that makes it long\n", i))
	}
	if err := os.WriteFile(file, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	view := m.View()
	if !strings.Contains(view, "wide.go") {
		t.Fatal("expected the tree pane (showing wide.go) to remain visible alongside a file with wide lines")
	}
	if got := strings.Count(view, "\n"); got > 30 {
		t.Fatalf("got %d rendered lines, want at most the window height (30) — a wide line must not wrap and overflow the terminal", got)
	}
}

func TestClampBlockWidthTruncatesOverlongLinesWithoutWrapping(t *testing.T) {
	block := "short\n" + strings.Repeat("x", 50) + "\nalso short"
	out := clampBlockWidth(block, 20)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d physical lines, want 3 (no wrapping)", len(lines))
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w > 20 {
			t.Fatalf("line %d: width %d exceeds cap of 20 (%q)", i, w, l)
		}
	}
}

func TestClampBlockWidthIsANoOpForNonPositiveWidth(t *testing.T) {
	block := "anything\nhere"
	if got := clampBlockWidth(block, 0); got != block {
		t.Fatalf("got %q, want unchanged %q", got, block)
	}
}

func TestTabCyclesThroughAllThreePanesForwardAndBack(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.focus != focusTree {
		t.Fatalf("got initial focus %v, want focusTree", m.focus)
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.focus != focusEditor {
		t.Fatalf("got focus %v after one Tab, want focusEditor", m.focus)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.focus != focusTerminal {
		t.Fatalf("got focus %v after two Tabs, want focusTerminal", m.focus)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.focus != focusTree {
		t.Fatalf("got focus %v after three Tabs, want focusTree (wrapped)", m.focus)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = updated.(Model)
	if m.focus != focusTerminal {
		t.Fatalf("got focus %v after one Shift+Tab, want focusTerminal (wrapped backward)", m.focus)
	}
}

func TestFocusingTheTerminalStartsItExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> editor
	m = updated.(Model)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> terminal
	m = updated.(Model)
	if !m.terminals[m.activeTerminal].term.Started() {
		t.Fatal("expected focusing the terminal pane to start it")
	}
	if cmd == nil {
		t.Fatal("expected a command (the pty read loop kickoff) when the terminal starts")
	}

	// Tabbing away and back must not restart it.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> tree
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> editor
	m = updated.(Model)
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> terminal again
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("expected no new start command the second time the terminal gains focus")
	}
}

func TestEditingShortcutsPassThroughToTheTerminalWhenItHasFocus(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // editor -> terminal
	m = updated.(Model)
	if m.focus != focusTerminal {
		t.Fatalf("got focus %v, want focusTerminal", m.focus)
	}

	before := m.recentCommand
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(Model)
	if m.recentCommand != before {
		t.Fatalf("expected ctrl+c to NOT trigger the Copy command while the terminal has focus, got recentCommand=%q", m.recentCommand)
	}
}

func TestOpenAndQuitStayGlobalEvenWhenTerminalHasFocus(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.focus != focusTerminal {
		t.Fatalf("got focus %v, want focusTerminal", m.focus)
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlQ})
	if cmd == nil {
		t.Fatal("expected ctrl+q to still produce tea.Quit while the terminal has focus")
	}
}

func TestTerminalPaneShowsRealShellOutputThroughTheComposedApp(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess; skipped in -short mode")
	}
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.terminals[m.activeTerminal].term.Close() })
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // tree -> editor
	m = updated.(Model)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab}) // editor -> terminal
	m = updated.(Model)
	if m.focus != focusTerminal {
		t.Fatalf("got focus %v, want focusTerminal", m.focus)
	}
	if cmd == nil {
		t.Fatal("expected a read-loop command once the terminal starts")
	}

	// Wait for the shell's startup prompt before typing, so the typed
	// command reaches a live shell rather than being sent before its
	// stdin is being read — same technique as Task 2's package-level
	// integration test.
	msg := cmd()
	out, ok := msg.(terminal.OutputMsg)
	if !ok {
		t.Fatalf("got %T as the first message, want terminal.OutputMsg (the shell prompt)", msg)
	}
	updated, cmd = m.Update(out)
	m = updated.(Model)

	// Type the command through the exact same tea.KeyMsg path a real
	// keystroke takes, one rune at a time, then Enter.
	command := "printf 'cody-app-marker\\n'"
	for _, r := range command {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if cmd == nil {
			t.Fatalf("read loop stopped before the marker appeared; last view:\n%s", m.View())
		}
		msg := cmd()
		out, ok := msg.(terminal.OutputMsg)
		if !ok {
			t.Fatalf("got %T, want terminal.OutputMsg", msg)
		}
		updated, cmd = m.Update(out)
		m = updated.(Model)
		if strings.Contains(m.View(), "cody-app-marker") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the marker text through the composed app view; last view:\n%s", m.View())
		}
	}
}

func TestWheelOverTerminalPaneScrollsIntoRealScrollback(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess; skipped in -short mode")
	}
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.terminals[m.activeTerminal].term.Close() })
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // tree -> editor
	m = updated.(Model)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab}) // editor -> terminal
	m = updated.(Model)

	msg := cmd()
	out, ok := msg.(terminal.OutputMsg)
	if !ok {
		t.Fatalf("got %T as the first message, want terminal.OutputMsg (the shell prompt)", msg)
	}
	updated, cmd = m.Update(out)
	m = updated.(Model)

	// Produce more output lines than the terminal pane's own content
	// height (interior height = terminalHeight(8) - border(2) -
	// tabBarHeight(1) = 5 rows) — enough that the earliest line
	// ("scrollback-row-1") is no longer visible in the live view at all,
	// only reachable by scrolling.
	command := "for i in 1 2 3 4 5 6 7 8 9 10; do echo scrollback-row-$i; done"
	for _, r := range command {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if cmd == nil {
			t.Fatalf("read loop stopped before scrollback-row-10 appeared; last view:\n%s", m.View())
		}
		msg := cmd()
		out, ok := msg.(terminal.OutputMsg)
		if !ok {
			t.Fatalf("got %T, want terminal.OutputMsg", msg)
		}
		updated, cmd = m.Update(out)
		m = updated.(Model)
		if strings.Contains(m.View(), "scrollback-row-10") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for scrollback-row-10 through the composed app view; last view:\n%s", m.View())
		}
	}

	if strings.Contains(m.View(), "scrollback-row-1\n") || strings.Contains(m.View(), "scrollback-row-1 ") {
		t.Fatalf("test setup: expected scrollback-row-1 to have already scrolled out of the live view before scrolling; view:\n%s", m.View())
	}

	// Scroll up one wheel notch at a time, checking after each, rather
	// than assuming a fixed notch count lands exactly on
	// scrollback-row-1 — a real shell's own startup banner (this
	// machine's .zshrc emits a couple of long, path-wrapping error
	// lines) can push the exact scroll distance around, since it adds an
	// unpredictable, environment-specific amount of content ahead of
	// anything this test actually typed. A generous notch budget still
	// bounds the loop so a genuine regression (scrolling not working at
	// all) fails promptly instead of hanging.
	_, _, term := m.paneLayout()
	x, y := term.terminal.x0+2, term.terminal.y0+1
	// A bare "scrollback-row-1" substring also matches "scrollback-row-10",
	// which the setup above already confirmed is on screen — checking for
	// that alone would let this test pass on the very first notch without
	// scrolling having done anything. Match the same newline/space-bounded
	// form the setup assertion above uses instead.
	rowOneVisible := func(view string) bool {
		return strings.Contains(view, "scrollback-row-1\n") || strings.Contains(view, "scrollback-row-1 ")
	}
	found := false
	for i := 0; i < 30; i++ {
		updated, _ = m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
		m = updated.(Model)
		if rowOneVisible(m.View()) {
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("expected scrolling the wheel over the terminal pane to reveal scrollback-row-1 within 30 notches, want it visible; last view:\n%s", m.View())
	}
}

func TestSearchingAndCyclingMatchesThroughTheComposedApp(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	src := "package main\n\nfunc add(a, b int) int {\n\treturn a + b\n}\n\nfunc addTwo(x int) int {\n\treturn add(x, 2)\n}\n"
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	m = updated.(Model)
	if m.activeDialog != dialogSearch {
		t.Fatalf("got activeDialog=%v, want dialogSearch", m.activeDialog)
	}

	for _, r := range "add" {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(Model)
	}
	line, _ := m.activeEditor().Cursor()
	if line != 3 {
		t.Fatalf("got cursor line=%d, want 3 (the first \"add\" match, in \"func add(\")", line)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	line, _ = m.activeEditor().Cursor()
	if line != 7 {
		t.Fatalf("got cursor line=%d, want 7 (the second \"add\" match, in \"func addTwo(\")", line)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.activeDialog != dialogNone {
		t.Fatalf("got activeDialog=%v, want dialogNone", m.activeDialog)
	}
}

// Pane geometry for an 80x24 window (see paneLayout): menu bar at y=0, body
// starting at y=1 with height 22 (no dropdown open). Tree occupies x[0,30),
// editor x[30,80) y[1,15), terminal x[30,80) y[15,23).

func TestClickInTreePaneFocusesTreeAndSelectsRow(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"0.go", "1.go", "2.go", "3.go", "4.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(""), 0644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	m.focus = focusEditor

	// row = y(5) - bodyTop(1) - border(1) = 3 -> "3.go".
	updated, cmd := m.Update(tea.MouseMsg{X: 5, Y: 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)
	if m.focus != focusTree {
		t.Fatal("expected clicking the tree pane to focus it")
	}
	if cmd == nil {
		t.Fatal("expected clicking a file row to activate it (open the file)")
	}
	msg := cmd()
	opened, ok := msg.(filetree.FileOpenedMsg)
	if !ok {
		t.Fatalf("got %T, want FileOpenedMsg", msg)
	}
	if filepath.Base(opened.Path) != "3.go" {
		t.Fatalf("got %q, want 3.go (row 3)", filepath.Base(opened.Path))
	}
}

func TestClickInEditorPanePositionsCursor(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "test.go")
	src := "line0\nline1\nline2\nline3\nline4\n"
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)
	m.focus = focusTree

	// x=43 -> relX = 43 - 30(editor x0) - 1(border) = 12 -> col = 12 - editorGutterWidth(7) = 5.
	// y=5  -> relY = 5 - 1(bodyTop) - 1(tab bar, one file is open) - 1(border) = 2 -> buffer line index 2 ("line2").
	updated, _ = m.Update(tea.MouseMsg{X: 43, Y: 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)
	if m.focus != focusEditor {
		t.Fatal("expected clicking the editor pane to focus it")
	}
	line, col := m.activeEditor().Cursor()
	if line != 3 || col != 6 {
		t.Fatalf("got line=%d col=%d, want 3,6 (line2, zero-indexed col 5)", line, col)
	}
}

// TestScrollThenClickInNewTabPositionsCursorAtVisibleLine reproduces the
// Critical-2 bug where a tab's editor, freshly created by openOrSwitch, was
// never given a size (width/height stayed 0). With height <= 0,
// editor.Model treats itself as "unbounded": ensureCursorVisible no-ops, so
// wheel-scrolling never advances scrollOffset even though the cursor (and
// therefore what's actually rendered) moves forward — and HandleClick, also
// gated on height > 0, then resolves a click at row y to line y instead of
// scrollOffset+y, landing far from the visibly rendered line.
func TestScrollThenClickInNewTabPositionsCursorAtVisibleLine(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "big.txt")
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, fmt.Sprintf("line%03d", i))
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	// Editor content height: paneHeight(24-1-1=22) - terminalHeight(8) -
	// tabBarHeight(1) - borderSize(2) = 11 rows.
	// 10 wheel notches * mouseWheelLines(3) = 30 lines moves the cursor to
	// line 30 (0-indexed), which is well past the 11-row viewport, forcing
	// ensureCursorVisible to scroll: scrollOffset = 30 - 11 + 1 = 20.
	for i := 0; i < 10; i++ {
		updated, _ = m.Update(tea.MouseMsg{X: 45, Y: 5, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = updated.(Model)
	}

	// Click at relY=5 within the editor pane. editorRect.y0 = bodyTop(1) +
	// tabBarH(1) = 2; screen y = editorRect.y0 + border(1) + relY.
	const relY = 5
	updated, _ = m.Update(tea.MouseMsg{X: 43, Y: 2 + 1 + relY, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)

	line, _ := m.activeEditor().Cursor()
	const wantLine = 20 + relY + 1 // scrollOffset(20) + relY, 1-indexed
	if line != wantLine {
		t.Fatalf("got cursor line=%d, want %d (scrollOffset 20 + relY %d, 1-indexed) — a newly opened tab's editor must be sized so scroll/click math isn't computed as if unbounded", line, wantLine, relY)
	}
}

func TestClickInTerminalPaneFocusesTerminal(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	// y=18 lands inside the terminal rect (y in [15, 23)).
	updated, _ = m.Update(tea.MouseMsg{X: 45, Y: 18, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)
	if m.focus != focusTerminal {
		t.Fatal("expected clicking the terminal pane to focus it")
	}
	if !m.terminals[m.activeTerminal].term.Started() {
		t.Fatal("expected the terminal to lazily start on first focus")
	}
}

func TestWheelOverTreeScrollsWithoutChangingFocus(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("%02d.go", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(""), 0644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	m.focus = focusEditor

	before := m.tree.View()
	if !strings.Contains(before, "00.go") {
		t.Fatal("setup failed: expected 00.go visible initially")
	}

	// 8 notches * mouseWheelLines(3) = 24 rows, past the ~20-row viewport,
	// forcing the tree to scroll.
	for i := 0; i < 8; i++ {
		updated, _ = m.Update(tea.MouseMsg{X: 5, Y: 5, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = updated.(Model)
	}
	if m.focus != focusEditor {
		t.Fatal("expected wheel scrolling not to change focus")
	}
	after := m.tree.View()
	if strings.Contains(after, "00.go") {
		t.Fatal("expected 00.go to have scrolled out of view")
	}
}

func TestOpeningTwoDifferentFilesCreatesTwoTabs(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)

	if len(m.panes[0].tabs) != 2 {
		t.Fatalf("got %d tabs, want 2", len(m.panes[0].tabs))
	}
	if m.panes[0].activeTab != 1 {
		t.Fatalf("got activeTab=%d, want 1 (the most recently opened)", m.panes[0].activeTab)
	}
}

func TestOpeningAnAlreadyOpenFileSwitchesInsteadOfDuplicating(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	// Move the cursor in fileB's tab so we can confirm re-opening fileA and
	// coming back to fileB preserves this, rather than reloading it.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	lineBeforeReopen, _ := m.activeEditor().Cursor()

	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	if len(m.panes[0].tabs) != 2 {
		t.Fatalf("got %d tabs, want 2 (re-opening fileA must not duplicate it)", len(m.panes[0].tabs))
	}
	if m.panes[0].activeTab != 0 {
		t.Fatalf("got activeTab=%d, want 0 (fileA's existing tab)", m.panes[0].activeTab)
	}

	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	if m.panes[0].activeTab != 1 {
		t.Fatalf("got activeTab=%d, want 1 (back to fileB's existing tab)", m.panes[0].activeTab)
	}
	lineAfterReturn, _ := m.activeEditor().Cursor()
	if lineAfterReturn != lineBeforeReopen {
		t.Fatalf("got cursor line=%d, want %d — switching back to an already-open tab must preserve its state, not reload it", lineAfterReturn, lineBeforeReopen)
	}
}

func TestClickingATabLabelSwitchesActiveTab(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	// activeTab == 1 (b.go). Tab bar is at y=1 (bodyTop, no dropdown open).
	// Tab 0's label starts at x=0 within the editor column (x=30 on screen).
	m.focus = focusTree

	updated, _ = m.Update(tea.MouseMsg{X: 30, Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)

	if m.panes[0].activeTab != 0 {
		t.Fatalf("got activeTab=%d, want 0", m.panes[0].activeTab)
	}
	if m.focus != focusEditor {
		t.Fatal("expected clicking a tab label to focus the editor")
	}
}

func TestClickingATabsCloseGlyphClosesIt(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	region := tabRegions(m.panes[0].tabs, 200)[0]
	// closeStart is relative to the tab bar's own x0 (defaultTreeWidth); the
	// screen column is defaultTreeWidth + closeStart.
	updated, _ = m.Update(tea.MouseMsg{X: defaultTreeWidth + region.closeStart, Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)

	if len(m.panes[0].tabs) != 0 {
		t.Fatal("expected clicking the close glyph on a clean tab to close it")
	}
}

func TestTreeShowsModifiedIndicatorForDirtyOpenTab(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	m = openAndDirtyFile(t, m, file)

	view := m.View()
	if !strings.Contains(view, "(M)") {
		t.Fatal("expected the tree to show a modified indicator for the dirty open file")
	}
}

func TestRightClickInTreeOpensContextMenu(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	m.focus = focusEditor

	updated, _ = m.Update(tea.MouseMsg{X: 5, Y: 5, Button: tea.MouseButtonRight, Action: tea.MouseActionPress})
	m = updated.(Model)

	if m.focus != focusTree {
		t.Fatal("expected right-clicking the tree to focus it")
	}
	if !strings.Contains(m.tree.View(), "New File") {
		t.Fatal("expected right-clicking the tree to open a context menu showing New File")
	}
}

func TestFileTreeErrorMsgSetsRecentCommand(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(filetree.FileTreeErrorMsg{Message: "boom"})
	m = updated.(Model)
	if m.recentCommand != "boom" {
		t.Fatalf("got recentCommand=%q, want %q", m.recentCommand, "boom")
	}
}

func TestNewWithRelativePathResolvesTreeRootToAbsolute(t *testing.T) {
	dir := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	m, err := New(".", false)
	if err != nil {
		t.Fatal(err)
	}
	if m.tree.SelectedDir() != m.rootPath {
		t.Fatalf("got tree root %q, want it to match m.rootPath %q — a relative launch path must resolve consistently for both", m.tree.SelectedDir(), m.rootPath)
	}
}

// setupSizedApp returns a Model sized to 80x24 with no tabs open, so the
// pane geometry (tree/editor/terminal rects) matches the values worked out
// in the resize tests below.
func setupSizedApp(t *testing.T) Model {
	t.Helper()
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return updated.(Model)
}

// readCmdOrTimeout runs cmd (a tea.Cmd whose blocking work is a real pty
// Read — there's no fake seam for it across the internal/terminal package
// boundary) in a goroutine and returns its result, or fails the test if it
// doesn't return within d. Guards tests that drive a real pty's blocking
// Read against hanging forever (rather than failing promptly) if the
// behavior they're checking for ever regresses.
func readCmdOrTimeout(t *testing.T, cmd tea.Cmd, d time.Duration) tea.Msg {
	t.Helper()
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg
	case <-time.After(d):
		t.Fatalf("cmd did not return within %s", d)
		return nil
	}
}

func press(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
}

func drag(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion}
}

func release(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonNone, Action: tea.MouseActionRelease}
}

func TestDraggingTreeBorderResizesTreeWidth(t *testing.T) {
	m := setupSizedApp(t)
	if m.treeWidth != defaultTreeWidth {
		t.Fatalf("got treeWidth=%d, want default %d", m.treeWidth, defaultTreeWidth)
	}

	updated, _ := m.Update(press(defaultTreeWidth-1, 5)) // tree's right border column
	m = updated.(Model)
	if m.resizeDrag != resizeTree {
		t.Fatalf("got resizeDrag=%v, want resizeTree", m.resizeDrag)
	}

	updated, _ = m.Update(drag(50, 5))
	m = updated.(Model)
	if m.treeWidth != 51 {
		t.Fatalf("got treeWidth=%d, want 51 (border tracks the pointer)", m.treeWidth)
	}

	updated, _ = m.Update(release(50, 5))
	m = updated.(Model)
	if m.resizeDrag != resizeNone {
		t.Fatal("expected release to end the drag")
	}
	if m.treeWidth != 51 {
		t.Fatalf("got treeWidth=%d, want it to stay at 51 after release", m.treeWidth)
	}
}

func TestDraggingTreeBorderClampsToMinAndMaxWidth(t *testing.T) {
	m := setupSizedApp(t)
	updated, _ := m.Update(press(defaultTreeWidth-1, 5))
	m = updated.(Model)

	updated, _ = m.Update(drag(2, 5))
	m = updated.(Model)
	if m.treeWidth != minTreeWidth {
		t.Fatalf("got treeWidth=%d, want it clamped to minTreeWidth=%d", m.treeWidth, minTreeWidth)
	}

	updated, _ = m.Update(drag(75, 5))
	m = updated.(Model)
	want := m.width - minEditorWidth
	if m.treeWidth != want {
		t.Fatalf("got treeWidth=%d, want it clamped to width-minEditorWidth=%d", m.treeWidth, want)
	}
}

func TestDraggingEditorTerminalBoundaryResizesTerminalHeight(t *testing.T) {
	m := setupSizedApp(t)
	if m.terminalHeight != defaultTerminalHeight {
		t.Fatalf("got terminalHeight=%d, want default %d", m.terminalHeight, defaultTerminalHeight)
	}
	_, panes, _ := m.paneLayout()
	boundaryY := panes[0].editor.y1 - 1 // editor's own bottom border row

	updated, _ := m.Update(press(40, boundaryY))
	m = updated.(Model)
	if m.resizeDrag != resizeTerminal {
		t.Fatalf("got resizeDrag=%v, want resizeTerminal", m.resizeDrag)
	}

	// Drag up 5 rows: terminal grows by exactly 5 (its top boundary moves
	// toward the tab bar). Computed independently of applyResizeDrag's own
	// term.terminal.y1-based formula (starting height + rows moved), not
	// re-derived through it — a re-derivation would silently pass even if
	// the anchor-row offset bug this test guards against (applyResizeDrag's
	// -1, see its own doc comment) were removed.
	updated, _ = m.Update(drag(40, boundaryY-5))
	m = updated.(Model)
	wantHeight := defaultTerminalHeight + 5
	if m.terminalHeight != wantHeight {
		t.Fatalf("got terminalHeight=%d, want %d (default %d + 5 rows dragged)", m.terminalHeight, wantHeight, defaultTerminalHeight)
	}

	updated, _ = m.Update(release(40, boundaryY-5))
	m = updated.(Model)
	if m.resizeDrag != resizeNone {
		t.Fatal("expected release to end the drag")
	}
	if m.terminalHeight != wantHeight {
		t.Fatalf("got terminalHeight=%d, want it to stay at %d after release", m.terminalHeight, wantHeight)
	}
}

// TestDraggingEditorTerminalBoundaryWithNoPointerMovementLeavesHeightUnchanged
// covers a pre-existing off-by-one Greptile's bot review caught on PR #6:
// applyResizeDrag measured the candidate height as term.terminal.y1-y with
// no adjustment, but the drag's only live trigger row (editorRect.y1-1,
// since the terminal's own tab bar row is tab-click territory, not a
// resize trigger, after this branch's own earlier fix) sits one row above
// where term.terminal.y1-term.tabBar.y0 actually equals the CURRENT
// terminalHeight — so pressing and releasing at that row without moving
// the pointer at all silently grew the terminal by one row every time.
func TestDraggingEditorTerminalBoundaryWithNoPointerMovementLeavesHeightUnchanged(t *testing.T) {
	m := setupSizedApp(t)
	if m.terminalHeight != defaultTerminalHeight {
		t.Fatalf("test setup: got terminalHeight=%d, want default %d", m.terminalHeight, defaultTerminalHeight)
	}
	_, panes, _ := m.paneLayout()
	boundaryY := panes[0].editor.y1 - 1

	updated, _ := m.Update(press(40, boundaryY))
	m = updated.(Model)
	// A motion event at the SAME coordinate as the press — applyResizeDrag
	// only ever runs on a motion event (press/release don't call it, see
	// Update's MouseActionRelease case), so this is the minimal event that
	// actually exercises the bug: zero net displacement from the press.
	updated, _ = m.Update(drag(40, boundaryY))
	m = updated.(Model)
	updated, _ = m.Update(release(40, boundaryY))
	m = updated.(Model)

	if m.terminalHeight != defaultTerminalHeight {
		t.Fatalf("got terminalHeight=%d after a drag with zero net pointer movement, want it unchanged at %d", m.terminalHeight, defaultTerminalHeight)
	}
}

func TestDraggingEditorTerminalBoundaryClampsToMinAndMaxHeight(t *testing.T) {
	m := setupSizedApp(t)
	_, panes, _ := m.paneLayout()
	boundaryY := panes[0].editor.y1 - 1 // editor's own bottom border row

	updated, _ := m.Update(press(40, boundaryY))
	m = updated.(Model)

	// Drag far up: terminal would grow past what leaves the editor its
	// minimum height, so it must clamp instead.
	updated, _ = m.Update(drag(40, 1))
	m = updated.(Model)
	paneHeight := m.height - menuBarHeight - statusBarHeight
	wantMax := paneHeight - m.tabBarH() - minEditorHeight
	if m.terminalHeight != wantMax {
		t.Fatalf("got terminalHeight=%d, want it clamped to %d", m.terminalHeight, wantMax)
	}

	// Drag far down: terminal shrinks to its minimum.
	updated, _ = m.Update(drag(40, 22))
	m = updated.(Model)
	if m.terminalHeight != minTerminalHeight {
		t.Fatalf("got terminalHeight=%d, want it clamped to minTerminalHeight=%d", m.terminalHeight, minTerminalHeight)
	}
}

func TestClickingInsideTreePaneDoesNotStartAResize(t *testing.T) {
	m := setupSizedApp(t)
	updated, _ := m.Update(press(10, 5)) // well inside the tree pane, not on its border
	m = updated.(Model)
	if m.resizeDrag != resizeNone {
		t.Fatal("expected a click inside the pane (not on its border) to not start a resize")
	}
	if m.focus != focusTree {
		t.Fatal("expected the click to still be routed to the tree pane as a normal click")
	}
}

func TestResizeDragDoesNotStartWhileADialogIsOpen(t *testing.T) {
	m := setupSizedApp(t)
	m.activeDialog = dialogAbout
	updated, _ := m.Update(press(defaultTreeWidth-1, 5))
	m = updated.(Model)
	if m.resizeDrag != resizeNone {
		t.Fatal("expected a border press to not start a resize while a dialog is open")
	}
}

// Regression test: clampTreeWidth's degenerate branch (see clampInt) pins
// treeWidth to minTreeWidth whenever the window is too small to honor both
// minTreeWidth and minEditorWidth at once — but it does so unconditionally,
// without regard to how small the window actually is. A window shrunk far
// enough (aggressive resizing) can still leave m.width - m.treeWidth -
// borderSize negative even after that clamp. That negative value used to
// flow straight into m.terminals[...].term.SetSize -> the real vt.Emulator's Resize,
// which panics on a negative slice bound rather than degrading gracefully.
// The started terminal (a real pty/shell, matching how this bug only
// reproduced once the terminal pane had actually been used) is required to
// reach that exact panic path — a never-started terminal's SetSize is a
// no-op past storing width/height.
func TestAggressiveResizeToTinyWidthDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> editor
	m = updated.(Model)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> terminal, starts it
	m = updated.(Model)
	if cmd != nil {
		cmd() // run the pty-start command so the terminal is actually started
	}
	if !m.terminals[m.activeTerminal].term.Started() {
		t.Fatal("setup failed: expected the terminal to be started")
	}
	t.Cleanup(func() { m.terminals[m.activeTerminal].term.Close() })

	// width=12 reproduces the reported crash exactly: with the default
	// treeWidth=30 pinned to minTreeWidth=15 by clampTreeWidth's degenerate
	// branch (clampInt), m.width-m.treeWidth-borderSize(2) = 12-15-2 = -5,
	// the exact panic value observed. treeWidth itself staying pinned at
	// 15 here is expected — the fix floors the *derived* size handed to
	// each pane's SetSize, not treeWidth's own clamped value, so this test
	// only asserts what the fix actually guarantees: reaching this line at
	// all, without the real vt.Emulator panicking on a negative resize.
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 12, Height: 24})
	m = updated.(Model) // must not panic

	if m.width != 12 {
		t.Fatalf("got m.width=%d, want 12 (the resize itself should still apply)", m.width)
	}
}

func TestMoveTabToOtherPaneCreatesSplitAndMovesTheTab(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	// pane 0 = [a.go, b.go], activeTab = 1 (b.go)

	m = m.moveTabToOtherPane(0, 0) // move a.go right

	if len(m.panes) != 2 {
		t.Fatalf("got %d panes, want 2", len(m.panes))
	}
	if len(m.panes[0].tabs) != 1 || m.panes[0].tabs[0].path != fileB {
		t.Fatalf("got pane0 tabs=%v, want just b.go", m.panes[0].tabs)
	}
	if len(m.panes[1].tabs) != 1 || m.panes[1].tabs[0].path != fileA {
		t.Fatalf("got pane1 tabs=%v, want just a.go", m.panes[1].tabs)
	}
	if m.activePane != 1 {
		t.Fatalf("got activePane=%d, want 1 (focus follows the moved tab)", m.activePane)
	}
	if m.focus != focusEditor {
		t.Fatal("expected focus to be on the editor after the move")
	}
}

func TestMoveTabToOtherPaneOfOnlyOpenTabCollapsesBackToOnePane(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	m = m.moveTabToOtherPane(0, 0)

	if len(m.panes) != 1 {
		t.Fatalf("got %d panes, want 1 (moving your only tab has nothing to split against)", len(m.panes))
	}
	if len(m.panes[0].tabs) != 1 || m.panes[0].tabs[0].path != file {
		t.Fatalf("got pane0 tabs=%v, want the tab still there", m.panes[0].tabs)
	}
	if m.activePane != 0 {
		t.Fatalf("got activePane=%d, want 0", m.activePane)
	}
}

func TestMoveTabToOtherPaneIntoAlreadySplitPaneAppends(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	fileC := filepath.Join(dir, "c.go")
	for _, f := range []string{fileA, fileB, fileC} {
		if err := os.WriteFile(f, []byte("package p"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	for _, f := range []string{fileA, fileB, fileC} {
		updated, _ = m.Update(filetree.FileOpenedMsg{Path: f})
		m = updated.(Model)
	}
	// pane0 = [a, b, c]. Split: move a.go right.
	m = m.moveTabToOtherPane(0, 0)
	// pane0 = [b, c], pane1 = [a]. Now move b.go right too.
	m = m.moveTabToOtherPane(0, 0)

	if len(m.panes[0].tabs) != 1 || m.panes[0].tabs[0].path != fileC {
		t.Fatalf("got pane0 tabs=%v, want just c.go", m.panes[0].tabs)
	}
	if len(m.panes[1].tabs) != 2 || m.panes[1].tabs[0].path != fileA || m.panes[1].tabs[1].path != fileB {
		t.Fatalf("got pane1 tabs=%v, want [a.go, b.go] (appended, not disturbing a.go)", m.panes[1].tabs)
	}
}

func TestMoveTabToOtherPaneCanMoveBackLeft(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 0) // pane0=[b], pane1=[a]

	m = m.moveTabToOtherPane(1, 0) // move a.go back left

	if len(m.panes) != 1 {
		t.Fatalf("got %d panes, want 1 (pane1 emptied, collapses)", len(m.panes))
	}
	if len(m.panes[0].tabs) != 2 || m.panes[0].tabs[0].path != fileB || m.panes[0].tabs[1].path != fileA {
		t.Fatalf("got pane0 tabs=%v, want [b.go, a.go] (b.go untouched, a.go appended back)", m.panes[0].tabs)
	}
	if m.activePane != 0 {
		t.Fatalf("got activePane=%d, want 0", m.activePane)
	}
}

func TestMoveTabToOtherPaneClosingPane0sOnlyTabWhilePane1SurvivesSwaps(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 0) // pane0=[b], pane1=[a]

	// Close pane0's only remaining tab directly (not via move) — exercises
	// removeTab's "pane 0 emptied, pane 1 survives" branch from Task 1.
	m, _ = m.closeTab(0, 0)

	if len(m.panes) != 1 {
		t.Fatalf("got %d panes, want 1", len(m.panes))
	}
	if len(m.panes[0].tabs) != 1 || m.panes[0].tabs[0].path != fileA {
		t.Fatalf("got pane0 tabs=%v, want just a.go (pane1's survivor swapped into slot 0)", m.panes[0].tabs)
	}
}

func TestSplitRendersTwoTabBarsAndTwoEditorBoxes(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a\nline in a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b\nline in b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 0)

	view := m.View()
	if !strings.Contains(view, "a.go") || !strings.Contains(view, "b.go") {
		t.Fatalf("expected both tab names visible in the split view")
	}
	if !strings.Contains(view, "line in a") || !strings.Contains(view, "line in b") {
		t.Fatalf("expected both files' content visible side by side")
	}
}

func TestDraggingSplitBoundaryResizesIt(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 0)
	startCol := m.splitCol

	_, panes, _ := m.paneLayout()
	boundaryX := panes[0].editor.x1
	updated, _ = m.Update(press(boundaryX, panes[0].editor.y0+1))
	m = updated.(Model)
	if m.resizeDrag != resizeEditorSplit {
		t.Fatalf("got resizeDrag=%v, want resizeEditorSplit", m.resizeDrag)
	}
	updated, _ = m.Update(drag(boundaryX+10, panes[0].editor.y0+1))
	m = updated.(Model)

	if m.splitCol != startCol+10 {
		t.Fatalf("got splitCol=%d, want %d", m.splitCol, startCol+10)
	}
}

func TestClickInPane1SwitchesActivePaneAndPositionsCursor(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("line0\nline1\nline2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 1) // b.go -> pane1, activePane=1
	m.activePane = 0               // simulate focus having moved back to pane0
	m.focus = focusEditor

	_, panes, _ := m.paneLayout()
	pl := panes[1]
	clickX := pl.editor.x0 + 1 + editorGutterWidthForTest()
	clickY := pl.editor.y0 + 1 + 1 // second visible row -> "line1"
	updated, _ = m.Update(tea.MouseMsg{X: clickX, Y: clickY, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)

	if m.activePane != 1 {
		t.Fatalf("got activePane=%d, want 1", m.activePane)
	}
	line, _ := m.activeEditor().Cursor()
	if line != 2 {
		t.Fatalf("got cursor line=%d, want 2 (line1, 1-indexed)", line)
	}
}

// editorGutterWidthForTest mirrors editor.editorGutterWidth (unexported,
// different package) for tests that need to click past the gutter.
func editorGutterWidthForTest() int { return 7 }

func TestTabCyclesThroughBothPanesWhenSplit(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 0) // split; activePane=1, focus=editor

	// Tab: editor(pane1) -> terminal (pane1 was already the "last" editor
	// stop reached by the split, so the very next Tab leaves the editor).
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.focus != focusTerminal {
		t.Fatalf("got focus=%v, want focusTerminal", m.focus)
	}

	// Tab: terminal -> tree -> editor(pane0) -> editor(pane1) -> terminal
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> tree
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> editor, pane0
	m = updated.(Model)
	if m.focus != focusEditor || m.activePane != 0 {
		t.Fatalf("got focus=%v activePane=%d, want focusEditor/0", m.focus, m.activePane)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> editor, pane1
	m = updated.(Model)
	if m.focus != focusEditor || m.activePane != 1 {
		t.Fatalf("got focus=%v activePane=%d, want focusEditor/1", m.focus, m.activePane)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> terminal
	m = updated.(Model)
	if m.focus != focusTerminal {
		t.Fatalf("got focus=%v, want focusTerminal", m.focus)
	}
}

func TestTabSkipsSecondPaneWhenUnsplit(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> editor
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> terminal directly, no second pane stop
	m = updated.(Model)
	if m.focus != focusTerminal {
		t.Fatalf("got focus=%v, want focusTerminal", m.focus)
	}
}

// End-to-end integration test covering the whole feature together: open
// two files, split, edit both independently, resize, move a tab back,
// close, quit-with-unsaved-changes still shows every dirty file across
// both panes.
func TestSplitViewEndToEnd(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 0) // pane0=[b], pane1=[a], activePane=1

	// Edit pane1's active tab (a.go).
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	m = updated.(Model)
	if !m.panes[1].tabs[0].editor.HasUnsavedChanges() {
		t.Fatal("expected editing pane1's tab to mark it dirty")
	}

	// Switch to pane0 and edit it too.
	m.activePane = 0
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Y")})
	m = updated.(Model)
	if !m.panes[0].tabs[0].editor.HasUnsavedChanges() {
		t.Fatal("expected editing pane0's tab to mark it dirty")
	}

	// Quitting should list both dirty files, from both panes.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlQ})
	m = updated.(Model)
	if m.activeDialog != dialogConfirmDiscard {
		t.Fatal("expected the quit confirmation to open with two dirty tabs across two panes")
	}

	// Cancel the quit, move the tab back left, close it clean.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	m = m.moveTabToOtherPane(1, 0) // a.go back to pane0
	if len(m.panes) != 1 {
		t.Fatalf("got %d panes, want 1 after moving pane1's only tab back", len(m.panes))
	}
	if len(m.panes[0].tabs) != 2 {
		t.Fatalf("got %d tabs in the collapsed pane, want 2", len(m.panes[0].tabs))
	}
}

// --- Final-review regression tests ---
//
// The four tests below cover the three Critical and one Important findings
// from the final whole-branch review (see
// .superpowers/sdd/2026-09-09-split-view-editor/final-review-fixes.md).

// TestRightClickedTabMenuClearsWhenWindowNarrowsPastItsAnchor covers
// Critical 1: m.tabMenu.index could exceed tabRegions(...)'s returned
// length after a resize narrowed the tab bar, and nothing bounds-checked
// before indexing — a guaranteed panic in View()/handleClick(). Fix 1 added
// tabMenuStartCol's ok-check; Fix 2 wired it into Update's WindowSizeMsg
// branch so a stale tab menu is actually cleared, not just safely ignored.
func TestRightClickedTabMenuClearsWhenWindowNarrowsPastItsAnchor(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("package p"), 0644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	m = updated.(Model)
	for _, p := range paths {
		updated, _ = m.Update(filetree.FileOpenedMsg{Path: p})
		m = updated.(Model)
	}
	if len(m.panes[0].tabs) != 8 {
		t.Fatalf("got %d tabs open, want 8", len(m.panes[0].tabs))
	}

	// Right-click the last tab (h.go, index 7). At width 200 the (unsplit)
	// tab bar is 170 cols wide (200 - defaultTreeWidth) — every 8-col tab
	// (" x.go × ") is visible.
	region := tabRegions(m.panes[0].tabs, 170)[7]
	if region.tabIndex != 7 {
		t.Fatalf("test setup: got region.tabIndex=%d, want 7 (h.go must be visible at full width)", region.tabIndex)
	}
	clickX := defaultTreeWidth + (region.startCol+region.endCol)/2
	updated, _ = m.Update(tea.MouseMsg{X: clickX, Y: 1, Button: tea.MouseButtonRight, Action: tea.MouseActionPress})
	m = updated.(Model)
	if m.tabMenu == nil || m.tabMenu.index != 7 {
		t.Fatalf("test setup: got tabMenu=%+v, want it open on index 7", m.tabMenu)
	}

	// Narrow the window enough that the (now 30-col) tab bar only fits 4
	// tabs — h.go's region (index 7) no longer exists.
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	m = updated.(Model)

	if m.tabMenu != nil {
		t.Fatalf("got tabMenu=%+v after narrowing past its anchor, want nil (Fix 2 must clear a stale tab menu on resize)", m.tabMenu)
	}

	// Confirm the full path stays panic-free end-to-end (Fix 1's
	// tabMenuStartCol ok-check is what actually prevents the crash if the
	// menu were somehow still set here).
	_ = m.View()
}

// TestRightClickedTabMenuInSplitViewFitsOnScreenAndItsClickTargetMatchesWhereItRenders
// covers Critical 2: the tab menu was appended as a trailing section after
// body without reducing bodyHeight (so the whole frame could exceed the
// terminal's height), and handleClick's hit-test row didn't match where the
// menu actually rendered (its only mouse trigger was effectively
// unusable).
func TestRightClickedTabMenuInSplitViewFitsOnScreenAndItsClickTargetMatchesWhereItRenders(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 0) // pane0=[b.go], pane1=[a.go]

	_, panes, _ := m.paneLayout()
	pl := panes[0]
	updated, _ = m.Update(tea.MouseMsg{X: pl.tabBar.x0 + 2, Y: pl.tabBar.y0, Button: tea.MouseButtonRight, Action: tea.MouseActionPress})
	m = updated.(Model)
	if m.tabMenu == nil || m.tabMenu.pane != 0 || m.tabMenu.index != 0 {
		t.Fatalf("test setup: got tabMenu=%+v, want it open on pane0/tab0", m.tabMenu)
	}

	// (a) The tab menu's own height must be reserved out of the body, so
	// the whole frame never exceeds the terminal's actual height.
	if h := lipgloss.Height(m.View()); h > 30 {
		t.Fatalf("got View() height=%d, want <= 30 (terminal height) — the tab menu must not be appended on top of an already-full-height body", h)
	}

	// (b) A click at exactly the (x, y) the menu itself computes as its
	// anchor/top must select its item — proves handleClick's hit-test
	// agrees with where the menu actually renders.
	startCol, ok := m.tabMenuStartCol()
	if !ok {
		t.Fatal("test setup: tabMenuStartCol() reported not ok")
	}
	top := m.tabMenuTop()
	// Verify top independently of tabMenuTop() itself — asserting only
	// top == m.tabMenuTop() would be tautological (handleClick's hit-test
	// also calls tabMenuTop(), so a wrong value there would agree with
	// itself and this test would still pass). No dropdown is open here,
	// so the row must be exactly menuBarHeight, and the rendered frame
	// must actually show the menu text on that row.
	if top != menuBarHeight {
		t.Fatalf("got tabMenuTop()=%d, want %d (menuBarHeight; no File/Edit dropdown is open)", top, menuBarHeight)
	}
	// The rendered box is bordered (dropdownStyle), so its text sits one
	// row below its own top border — find the text's actual row by
	// scanning the rendered frame directly (not via tabMenuTop()) and
	// check it lands exactly where top+1 says it should.
	lines := strings.Split(m.View(), "\n")
	textRow := -1
	for i, line := range lines {
		if strings.Contains(line, "Split + Move Right") {
			textRow = i
			break
		}
	}
	if textRow != top+1 {
		t.Fatalf("got the tab menu's text on rendered row %d, want row %d (tabMenuTop()+1, for the box's top border); rendered rows: %q", textRow, top+1, lines)
	}
	// Verify width independently of the hit-test's own hi bound too: the
	// visible box (rendered with no margin) must end well before the
	// terminal's right edge, and a click just past that visible box must
	// NOT select the item — proves the hit-test isn't using the
	// margin-inflated width (dropdownStyle.MarginLeft(startCol) baked
	// into renderTabMenu's own output).
	visibleWidth := lipgloss.Width(renderTabMenu(*m.tabMenu, 0))
	justPastVisibleBox := startCol + visibleWidth + 1
	if justPastVisibleBox >= m.width {
		t.Fatalf("test setup: startCol=%d visibleWidth=%d leaves no room past the box within width=%d", startCol, visibleWidth, m.width)
	}
	probeUpdated, _ := m.Update(tea.MouseMsg{X: justPastVisibleBox, Y: top, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	probe := probeUpdated.(Model)
	// A left click always clears m.tabMenu, whether it hit (select) or
	// missed (plain dismiss) — so the tell isn't tabMenu itself, it's
	// whether the item's action (moveTabToOtherPane, which here collapses
	// pane0 away since b.go is its only tab) actually ran.
	if probe.tabMenu != nil {
		t.Fatalf("test setup: click at x=%d didn't clear m.tabMenu at all", justPastVisibleBox)
	}
	if len(probe.panes) != len(m.panes) {
		t.Fatalf("click at x=%d (just past the visible menu box, which ends at %d) wrongly ran the tab menu's action (panes went from %d to %d) — hit-test width must not include the anchor margin", justPastVisibleBox, startCol+visibleWidth, len(m.panes), len(probe.panes))
	}

	beforePanes := len(m.panes)
	updated, _ = m.Update(tea.MouseMsg{X: startCol, Y: top, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)

	if m.tabMenu != nil {
		t.Fatal("expected the click at the menu's own computed anchor to select its item and close the menu")
	}
	// selectTabMenuItem("Split + Move Right") on pane0's only tab moves it
	// into pane1 (which already has a.go), emptying pane0 — removeTab's
	// collapse rule then merges back down to a single pane.
	if len(m.panes) != beforePanes-1 {
		t.Fatalf("got %d panes after selecting the tab menu item, want %d (collapsed back to one pane)", len(m.panes), beforePanes-1)
	}
	if len(m.panes[0].tabs) != 2 {
		t.Fatalf("got %d tabs in the collapsed pane, want 2 (a.go and b.go)", len(m.panes[0].tabs))
	}
}

// TestSplitColumnReClampsWhenWindowShrinksBelowItsOldPosition and
// TestSplitColumnReClampsWhenTreeBorderIsDraggedTowardIt cover Critical 3:
// m.splitCol was never re-clamped after a window resize or a tree-width
// drag, so either could push it past the new bounds and invert a pane's
// rect.

func TestSplitColumnReClampsWhenWindowShrinksBelowItsOldPosition(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 0) // split; splitCol defaults to the midpoint, 115
	if m.splitCol != 115 {
		t.Fatalf("test setup: got splitCol=%d, want 115 (midpoint at width 200)", m.splitCol)
	}

	updated, _ = m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	m = updated.(Model)

	lo := m.treeWidth + minEditorWidth
	hi := m.width - minEditorWidth
	if m.splitCol < lo || m.splitCol > hi {
		t.Fatalf("got splitCol=%d after shrinking the window, want it re-clamped to [%d, %d]", m.splitCol, lo, hi)
	}
	_, panes, _ := m.paneLayout()
	if panes[1].editor.x1 <= panes[1].editor.x0 {
		t.Fatalf("got panes[1] editor rect x0=%d x1=%d, want x1 > x0 (the right pane must not invert)", panes[1].editor.x0, panes[1].editor.x1)
	}
}

func TestSplitColumnReClampsWhenTreeBorderIsDraggedTowardIt(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 0) // split; splitCol defaults to 115
	if m.splitCol != 115 {
		t.Fatalf("test setup: got splitCol=%d, want 115", m.splitCol)
	}

	// Drag the tree border out to x=150 — well past the split boundary at
	// 115.
	updated, _ = m.Update(press(defaultTreeWidth-1, 5))
	m = updated.(Model)
	if m.resizeDrag != resizeTree {
		t.Fatalf("test setup: got resizeDrag=%v, want resizeTree", m.resizeDrag)
	}
	updated, _ = m.Update(drag(150, 5))
	m = updated.(Model)

	lo := m.treeWidth + minEditorWidth
	hi := m.width - minEditorWidth
	if m.splitCol < lo || m.splitCol > hi {
		t.Fatalf("got splitCol=%d after dragging the tree border past it, want it re-clamped to [%d, %d]", m.splitCol, lo, hi)
	}
	_, panes, _ := m.paneLayout()
	if panes[0].editor.x1 <= panes[0].editor.x0 {
		t.Fatalf("got panes[0] editor rect x0=%d x1=%d, want x1 > x0 (the left pane must not invert)", panes[0].editor.x0, panes[0].editor.x1)
	}
	if panes[1].editor.x1 <= panes[1].editor.x0 {
		t.Fatalf("got panes[1] editor rect x0=%d x1=%d, want x1 > x0 (the right pane must not invert)", panes[1].editor.x0, panes[1].editor.x1)
	}
}

// TestDraggingTerminalBoundaryUpdatesEditorViewportBeforeAnyClick covers a
// bot-review finding on the fix-wave diff itself (Greptile, PR #5):
// applyResizeDrag changed m.terminalHeight (and therefore the on-screen
// editorHeight) without pushing that new size into the pane's persisted
// editor.Model via SetSize — only WindowSizeMsg did that. View() itself
// always rendered correctly (it recomputes a fresh SetSize copy every
// frame), but a click landing before the next WindowSizeMsg computed
// against the editor's stale stored scrollOffset, targeting a different
// line than what was actually on screen at that row.
func TestDraggingTerminalBoundaryUpdatesEditorViewportBeforeAnyClick(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "big.txt")
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, fmt.Sprintf("line%03d", i))
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: file})
	m = updated.(Model)

	// Same setup as TestScrollThenClickInNewTabPositionsCursorAtVisibleLine:
	// paneHeight=22, terminalHeight=8, tabBarH=1, so the editor's content
	// height is 11 rows. 10 wheel notches * mouseWheelLines(3) moves the
	// cursor to (0-indexed) line 30, forcing scrollOffset to 30-11+1=20.
	for i := 0; i < 10; i++ {
		updated, _ = m.Update(tea.MouseMsg{X: 45, Y: 5, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = updated.(Model)
	}

	// Drag the terminal boundary to shrink the editor's content height from
	// 11 rows to 4: terminalRect.y1 = bodyTop(1) + paneHeight(22) = 23: to
	// land terminalHeight at 15 (giving editorHeight = 22-15-1 = 6 outer,
	// 4 interior after the 2-cell border), drag to y = 23-15-1 = 7 (the
	// -1 cancels applyResizeDrag's own anchor-row offset — see its doc
	// comment on the resizeTerminal case).
	m.resizeDrag = resizeTerminal
	m = m.applyResizeDrag(45, 7)
	if m.terminalHeight != 15 {
		t.Fatalf("test setup: got terminalHeight=%d after the drag, want 15", m.terminalHeight)
	}

	// If the drag correctly propagated into the editor's stored state,
	// ensureCursorVisible (cursor still at line 30, viewport now only 4
	// rows tall) re-clamps scrollOffset to 30-4+1=27 — line 27 is now the
	// top row. Verify independently, off the actually-rendered frame, not
	// by re-deriving the expected value through the same click path being
	// tested: search m.View() for "line027" and confirm it's the editor
	// pane's own top content row.
	lines2 := strings.Split(m.View(), "\n")
	bodyTop, tabBarH, border := 1, 1, 1
	topRow := bodyTop + tabBarH + border // = 3
	if topRow >= len(lines2) || !strings.Contains(lines2[topRow], "line027") {
		t.Fatalf("test setup: expected rendered row %d to show line027 after the drag (scrollOffset should re-clamp to 27); rendered rows: %q", topRow, lines2)
	}

	// A click at the pane's own top row (relY=0) must select the line
	// that's actually rendered there (line 27, 0-indexed — 28 as Cursor()
	// reports it, 1-indexed) — not line 20 (the pre-drag scrollOffset,
	// which the buggy code would still be using).
	updated, _ = m.Update(tea.MouseMsg{X: 45, Y: topRow, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)
	line, _ := m.activeEditor().Cursor()
	if line != 28 {
		t.Fatalf("got cursor line=%d after clicking the editor's top row post-drag, want 28 (line027, 0-indexed 27, 1-indexed 28) — the click must target what's actually rendered there, not a stale pre-drag scrollOffset", line)
	}
}

// TestSplitColumnNeverExceedsWidthOnAnAggressivelyShrunkWindow covers the
// final-review fix wave's re-review residual on Critical 3: on a window
// too small even for its own minTreeWidth, clampTreeWidth's own degenerate
// handling (see clampInt's hi<lo branch) can still leave m.treeWidth at
// minTreeWidth despite m.width having no room for it. Re-clamping splitCol
// from that inflated treeWidth then pushed clampSplitCol's own lo bound
// past m.width, and clampInt's hi<lo branch returned that lo verbatim —
// past the terminal's own width — inverting pane 1's rect. splitCol must
// never exceed m.width, however degenerate the window.
func TestSplitColumnNeverExceedsWidthOnAnAggressivelyShrunkWindow(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 0) // split; splitCol defaults to 115

	// Shrink well below minTreeWidth+minEditorWidth (35): clampTreeWidth's
	// degenerate branch forces treeWidth to minTreeWidth (15) even though
	// width=20 leaves no room for it.
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 20, Height: 30})
	m = updated.(Model)

	if m.splitCol > m.width {
		t.Fatalf("got splitCol=%d, width=%d — splitCol must never exceed the terminal's own width", m.splitCol, m.width)
	}
	widths := m.paneWidths()
	for i, w := range widths {
		if w < 0 {
			t.Fatalf("got paneWidths()[%d]=%d, want >= 0 (a negative pane width means its rect inverted)", i, w)
		}
	}
	_, panes, _ := m.paneLayout()
	for i, pl := range panes {
		if pl.editor.x1 < pl.editor.x0 {
			t.Fatalf("got panes[%d] editor rect x0=%d x1=%d, want x1 >= x0 (must not invert)", i, pl.editor.x0, pl.editor.x1)
		}
	}
}

// TestNewTabInNarrowerSplitPaneIsSizedToThatPanesOwnWidth covers the
// Important finding: newTabEditorSize (and the WindowSizeMsg per-tab resize
// loop) sized every tab's editor to the full editor column instead of its
// own pane's on-screen share, per spec §3.4.
func TestNewTabInNarrowerSplitPaneIsSizedToThatPanesOwnWidth(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	fileC := filepath.Join(dir, "c.go")
	if err := os.WriteFile(fileA, []byte("package a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package b"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileC, []byte("package c"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileA})
	m = updated.(Model)
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileB})
	m = updated.(Model)
	m = m.moveTabToOtherPane(0, 0) // pane0=[b.go], pane1=[a.go]; activePane=1

	// Push the split boundary far right so the two panes have visibly
	// different widths: pane0 = 140 cols, pane1 = 30 cols.
	m.splitCol = 170

	widths := m.paneWidths()
	if widths[0] == widths[1] {
		t.Fatalf("test setup: got equal pane widths %v, want them to differ", widths)
	}
	if m.activePane != 1 {
		t.Fatalf("test setup: got activePane=%d, want 1 (the narrower pane, from the move above)", m.activePane)
	}

	wantW := widths[1] - borderSize
	gotW, _ := m.newTabEditorSize()
	if gotW != wantW {
		t.Fatalf("got newTabEditorSize width=%d, want %d (pane1's own on-screen width, not the full editor column)", gotW, wantW)
	}
	if fullColumnW := m.width - m.treeWidth - borderSize; gotW == fullColumnW {
		t.Fatalf("got newTabEditorSize width=%d, same as the old full-editor-column formula (%d) — it must use the active pane's actual on-screen width instead", gotW, fullColumnW)
	}

	// Exercise it end-to-end too: opening a new tab must not panic and
	// must land in the active (narrower) pane.
	updated, _ = m.Update(filetree.FileOpenedMsg{Path: fileC})
	m = updated.(Model)
	if len(m.panes[1].tabs) != 2 {
		t.Fatalf("got %d tabs in pane1, want 2 (a.go, c.go)", len(m.panes[1].tabs))
	}
}

func TestNewStartsWithExactlyOneUnstartedTerminalTab(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.terminals) != 1 {
		t.Fatalf("got %d terminal tabs, want 1", len(m.terminals))
	}
	if m.activeTerminal != 0 {
		t.Fatalf("got activeTerminal=%d, want 0", m.activeTerminal)
	}
	if m.terminals[0].term.Started() {
		t.Fatal("expected the initial terminal tab to not be started yet (lazy start)")
	}
}

func TestCmdNewTerminalTabAppendsActivatesAndStartsIt(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	m.focus = focusTree // deliberately not focusTerminal yet

	m, _ = cmdNewTerminalTab(m)
	t.Cleanup(func() {
		for i := range m.terminals {
			m.terminals[i].term.Close()
		}
	})

	if len(m.terminals) != 2 {
		t.Fatalf("got %d terminal tabs, want 2", len(m.terminals))
	}
	if m.activeTerminal != 1 {
		t.Fatalf("got activeTerminal=%d, want 1 (the new tab)", m.activeTerminal)
	}
	if m.focus != focusTerminal {
		t.Fatal("expected cmdNewTerminalTab to focus the terminal pane")
	}
	if !m.terminals[1].term.Started() {
		t.Fatal("expected cmdNewTerminalTab to start the new tab's shell immediately, since it also focuses it")
	}
	if m.terminals[1].term.ID() == m.terminals[0].term.ID() {
		t.Fatalf("got both tabs' ID()=%d, want distinct ids", m.terminals[1].term.ID())
	}
}

// TestCmdNewTerminalTabSizesTheNewSessionBeforeStarting guards against the
// gap where a newly appended terminal tab was left at zero width/height:
// terminal.Model's own Start() falls back to a hardcoded 80x24 whenever its
// stored size is <= 0 (see that package's doc comment), so an unsized new
// tab would still start — just at the wrong size — silently masking the
// bug. Sizing it correctly via newTerminalSize() first means Start() uses
// the real pane geometry instead of that fallback. There's no width/height
// getter on terminal.Model to assert against directly, so this observes it
// through Render(): the real vt.Emulator renders exactly `height` lines
// (confirmed empirically — see the task's own note on this), so a session
// started at the 80x24 fallback instead of its real ~5-line pane would
// render 24 lines here, not 5.
func TestCmdNewTerminalTabSizesTheNewSessionBeforeStarting(t *testing.T) {
	m := setupSizedApp(t)
	wantW, wantH := m.newTerminalSize()
	if wantW <= 0 || wantH <= 0 {
		t.Fatalf("test setup: newTerminalSize returned non-positive (%d,%d) for an 80x24 window", wantW, wantH)
	}
	if wantH == 24 {
		t.Fatal("test setup: wantH coincides with terminal.Model's own 80x24 fallback height — this test can't distinguish the two at this window size")
	}

	m, _ = cmdNewTerminalTab(m)
	t.Cleanup(func() {
		for i := range m.terminals {
			m.terminals[i].term.Close()
		}
	})

	lines := strings.Split(m.terminals[m.activeTerminal].term.View(), "\n")
	if len(lines) != wantH {
		t.Fatalf("got %d rendered lines from the new tab's started shell, want %d (newTerminalSize's height) — suggests the session started at terminal.Model's 80x24 fallback instead of being sized first", len(lines), wantH)
	}
}

// TestRemoveTerminalTabSoleRemainingResetSizesTheReplacementSession mirrors
// the above for removeTerminalTab's sole-remaining-tab reset path (see
// confirm.go), which has the identical unsized-construction gap. The
// replacement session isn't started by removeTerminalTab itself (it stays
// freshly unstarted, per TestRemoveTerminalTabOnTheSoleRemainingTabResetsInPlaceInsteadOfEmptying),
// so this starts it manually afterward and checks the same rendered-line-
// count signal.
func TestRemoveTerminalTabSoleRemainingResetSizesTheReplacementSession(t *testing.T) {
	m := setupSizedApp(t)
	wantW, wantH := m.newTerminalSize()
	if wantW <= 0 || wantH <= 0 {
		t.Fatalf("test setup: newTerminalSize returned non-positive (%d,%d) for an 80x24 window", wantW, wantH)
	}
	if wantH == 24 {
		t.Fatal("test setup: wantH coincides with terminal.Model's own 80x24 fallback height — this test can't distinguish the two at this window size")
	}

	m = m.removeTerminalTab(0)
	m.terminals[0].term, _ = m.terminals[0].term.Start()
	t.Cleanup(func() { m.terminals[0].term.Close() })

	lines := strings.Split(m.terminals[0].term.View(), "\n")
	if len(lines) != wantH {
		t.Fatalf("got %d rendered lines from the replacement session's started shell, want %d (newTerminalSize's height) — suggests it was never sized before being started", len(lines), wantH)
	}
}

func TestCtrlTCreatesANewTerminalTabEvenWhileTheTerminalPaneAlreadyHasFocus(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	m.focus = focusTerminal // keys would otherwise route straight to the shell

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = updated.(Model)
	t.Cleanup(func() {
		for i := range m.terminals {
			m.terminals[i].term.Close()
		}
	})

	if len(m.terminals) != 2 {
		t.Fatalf("got %d terminal tabs after ctrl+t while terminal-focused, want 2", len(m.terminals))
	}
}

func TestRemoveTerminalTabReassignsActiveTerminalLikeRemoveTabDoesForEditorTabs(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// Grow to 3 tabs: [0, 1, 2], activeTerminal ends at 2.
	m, _ = cmdNewTerminalTab(m)
	m, _ = cmdNewTerminalTab(m)
	if len(m.terminals) != 3 || m.activeTerminal != 2 {
		t.Fatalf("test setup: got %d tabs, activeTerminal=%d, want 3 tabs, activeTerminal=2", len(m.terminals), m.activeTerminal)
	}

	t.Cleanup(func() {
		for i := range m.terminals {
			m.terminals[i].term.Close()
		}
	})

	// Closing a tab before the active one shifts activeTerminal left.
	m = m.removeTerminalTab(0)
	if len(m.terminals) != 2 || m.activeTerminal != 1 {
		t.Fatalf("got %d tabs, activeTerminal=%d after closing index 0, want 2 tabs, activeTerminal=1", len(m.terminals), m.activeTerminal)
	}

	// Closing the (now last, and active) tab falls back to the one before it.
	m = m.removeTerminalTab(1)
	if len(m.terminals) != 1 || m.activeTerminal != 0 {
		t.Fatalf("got %d tabs, activeTerminal=%d after closing the last active tab, want 1 tab, activeTerminal=0", len(m.terminals), m.activeTerminal)
	}
}

// TestRemoveTerminalTabAfterTheActiveOneLeavesActiveTerminalUnchanged covers
// the index > activeTerminal case missing from
// TestRemoveTerminalTabReassignsActiveTerminalLikeRemoveTabDoesForEditorTabs:
// closing a tab that comes AFTER the active one shouldn't move it.
func TestRemoveTerminalTabAfterTheActiveOneLeavesActiveTerminalUnchanged(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	m, _ = cmdNewTerminalTab(m)
	m, _ = cmdNewTerminalTab(m)
	t.Cleanup(func() {
		for i := range m.terminals {
			m.terminals[i].term.Close()
		}
	})
	if len(m.terminals) != 3 {
		t.Fatalf("test setup: got %d tabs, want 3", len(m.terminals))
	}
	// Simulate the user having switched back to tab 1 (the middle tab).
	m.activeTerminal = 1

	m = m.removeTerminalTab(2) // index 2 > activeTerminal 1

	if len(m.terminals) != 2 || m.activeTerminal != 1 {
		t.Fatalf("got %d tabs, activeTerminal=%d after closing a tab after the active one, want 2 tabs, activeTerminal=1 (unchanged)", len(m.terminals), m.activeTerminal)
	}
}

// TestRemoveTerminalTabAtActiveIndexWithALaterTabPresentKeepsActiveAtTheSameIndex
// covers the index == activeTerminal, tabs-remain-after-it case missing
// from TestRemoveTerminalTabReassignsActiveTerminalLikeRemoveTabDoesForEditorTabs
// — mirrors removeTab's own already-tested
// TestCloseTabReassignsActiveTabWhenLaterTabExists for editor tabs: closing
// the active tab with a later tab present shifts that later tab left into
// the closed slot, so activeTerminal stays at the same numeric index but
// now points at the tab that shifted into it.
func TestRemoveTerminalTabAtActiveIndexWithALaterTabPresentKeepsActiveAtTheSameIndex(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	m, _ = cmdNewTerminalTab(m)
	m, _ = cmdNewTerminalTab(m)
	t.Cleanup(func() {
		for i := range m.terminals {
			m.terminals[i].term.Close()
		}
	})
	if len(m.terminals) != 3 {
		t.Fatalf("test setup: got %d tabs, want 3", len(m.terminals))
	}
	survivorID := m.terminals[2].term.ID()
	m.activeTerminal = 1

	m = m.removeTerminalTab(1) // index == activeTerminal, tab 2 remains after it

	if len(m.terminals) != 2 || m.activeTerminal != 1 {
		t.Fatalf("got %d tabs, activeTerminal=%d after closing the active tab with a later tab present, want 2 tabs, activeTerminal=1", len(m.terminals), m.activeTerminal)
	}
	if m.terminals[m.activeTerminal].term.ID() != survivorID {
		t.Fatal("expected activeTerminal to now point at the tab that shifted into the closed slot, not some other tab")
	}
}

func TestRemoveTerminalTabOnTheSoleRemainingTabResetsInPlaceInsteadOfEmptying(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	oldID := m.terminals[0].term.ID()

	m = m.removeTerminalTab(0)

	if len(m.terminals) != 1 {
		t.Fatalf("got %d terminal tabs after closing the sole remaining one, want 1 (reset in place, never empty)", len(m.terminals))
	}
	if m.activeTerminal != 0 {
		t.Fatalf("got activeTerminal=%d, want 0", m.activeTerminal)
	}
	if m.terminals[0].term.ID() == oldID {
		t.Fatal("expected the replacement session to have a fresh id, not reuse the closed one's")
	}
	if m.terminals[0].term.Started() {
		t.Fatal("expected the replacement session to be freshly unstarted")
	}
}

func TestBackgroundTerminalTabOutputMsgIsRoutedToItsOwnEmulatorNotTheActiveOne(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	m.focus = focusTerminal

	m, _ = cmdNewTerminalTab(m) // tab 1 is now active

	// Start tab 0 (the backgrounded one) directly, bypassing focus, so it
	// has a live generation to accept an OutputMsg against.
	var cmd tea.Cmd
	m.terminals[0].term, cmd = m.terminals[0].term.Start()
	if cmd == nil {
		t.Fatal("test setup: starting tab 0 returned a nil cmd")
	}

	t.Cleanup(func() { m.terminals[0].term.Close() })
	t.Cleanup(func() { m.terminals[1].term.Close() })

	// Captured before applying any of tab 0's OutputMsgs, synchronously in
	// this same goroutine with no intervening call that could change
	// tab 1 — a bug that routed the write into both tabs' emulators would
	// change this.
	tab1ViewBefore := m.terminals[1].term.View()

	// A freshly spawned shell's first few pty reads are often invisible
	// escape/title sequences (e.g. zsh's own no-newline indicator, which
	// draws then immediately clears itself within one chunk) before its
	// prompt actually appears — so pump tab 0's output through Update
	// until its own rendered view shows real visible content, rather than
	// asserting on just the first chunk. Bounded so a stalled pty fails
	// this test outright instead of hanging it.
	const maxReads = 10
	for i := 0; i < maxReads && cmd != nil && strings.TrimSpace(m.terminals[0].term.View()) == ""; i++ {
		msg := readCmdOrTimeout(t, cmd, 5*time.Second)
		out, ok := msg.(terminal.OutputMsg)
		if !ok {
			t.Fatalf("got %T from tab 0's pty, want terminal.OutputMsg", msg)
		}
		var updated tea.Model
		updated, cmd = m.Update(out)
		m = updated.(Model)
	}

	if strings.TrimSpace(m.terminals[0].term.View()) == "" {
		t.Fatal("expected tab 0's OutputMsg(s) to have produced visible content, not silently dropped")
	}
	if m.terminals[1].term.View() != tab1ViewBefore {
		t.Fatal("expected tab 1 (not the tab the OutputMsg belongs to) to be left untouched by routing tab 0's OutputMsg")
	}
}

func TestQuitClosesEveryTerminalTabNotJustTheActiveOne(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// cmdNewTerminalTab starts tab 1 (the new tab) automatically since it
	// also focuses it — capture its read cmd from that call directly,
	// since calling Start() on an already-started session is a documented
	// no-op that returns a nil cmd.
	var cmd1 tea.Cmd
	m, cmd1 = cmdNewTerminalTab(m)

	var cmd0 tea.Cmd
	m.terminals[0].term, cmd0 = m.terminals[0].term.Start()
	if cmd0 == nil || cmd1 == nil {
		t.Fatal("test setup: Start() returned a nil read cmd")
	}

	m, _ = cmdQuit(m)

	if m.activeDialog != dialogNone {
		t.Fatal("expected a clean quit (no dirty editor tabs) to not open a dialog")
	}

	// Prove the underlying ptys were actually closed (not just that the
	// dialog-free path reached tea.Quit) by invoking each tab's own read
	// cmd, captured from Start() BEFORE cmdQuit ran. A closed pty's Read
	// returns an error immediately, which readCmd turns into a
	// terminal.ReadErrMsg; a still-open pty's Read would instead block
	// (there's no output from an idle shell), so readCmdOrTimeout bounds
	// each call rather than risking a hang if cmdQuit's close loop is ever
	// deleted or broken.
	msg0 := readCmdOrTimeout(t, cmd0, 2*time.Second)
	if _, ok := msg0.(terminal.ReadErrMsg); !ok {
		t.Fatalf("tab 0: got %T from its captured read cmd after cmdQuit, want terminal.ReadErrMsg", msg0)
	}
	msg1 := readCmdOrTimeout(t, cmd1, 2*time.Second)
	if _, ok := msg1.(terminal.ReadErrMsg); !ok {
		t.Fatalf("tab 1: got %T from its captured read cmd after cmdQuit, want terminal.ReadErrMsg", msg1)
	}
}

func TestTerminalTabBarIsAlwaysShownEvenWithOneTab(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)

	if !strings.Contains(m.View(), "Shell 1") {
		t.Fatal("expected the terminal tab bar to render \"Shell 1\" even with only one (unstarted) terminal tab")
	}
}

func TestClickingATerminalTabSwitchesActiveTerminalAndFocusesThePane(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	m, _ = cmdNewTerminalTab(m) // now 2 tabs, activeTerminal=1
	t.Cleanup(func() {
		for i := range m.terminals {
			m.terminals[i].term.Close()
		}
	})
	m.focus = focusTree

	_, _, term := m.paneLayout()
	region, ok := terminalTabAt(0, m.terminals, term.tabBar.x1-term.tabBar.x0)
	if !ok || region.tabIndex != 0 {
		t.Fatalf("test setup: got region=%+v ok=%v, want tabIndex=0 at column 0", region, ok)
	}
	clickX := term.tabBar.x0
	clickY := term.tabBar.y0

	updated, _ = m.Update(tea.MouseMsg{X: clickX, Y: clickY, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)

	if m.activeTerminal != 0 {
		t.Fatalf("got activeTerminal=%d after clicking tab 0, want 0", m.activeTerminal)
	}
	if m.focus != focusTerminal {
		t.Fatal("expected clicking a terminal tab to focus the terminal pane")
	}
}

func TestClickingATerminalTabsCloseGlyphRemovesItWithoutSwitchingFocus(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	m, _ = cmdNewTerminalTab(m) // 2 tabs, activeTerminal=1
	t.Cleanup(func() {
		for i := range m.terminals {
			m.terminals[i].term.Close()
		}
	})
	m.focus = focusTree

	_, _, term := m.paneLayout()
	regions := terminalTabRegions(m.terminals, term.tabBar.x1-term.tabBar.x0)
	closeX := term.tabBar.x0 + regions[0].closeStart
	closeY := term.tabBar.y0

	updated, _ = m.Update(tea.MouseMsg{X: closeX, Y: closeY, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)

	if len(m.terminals) != 1 {
		t.Fatalf("got %d terminal tabs after closing tab 0's ×, want 1", len(m.terminals))
	}
	if m.focus != focusTree {
		t.Fatal("expected closing a terminal tab (via its × ) to not change focus, matching the editor tab bar's own close-glyph precedent")
	}
}

// TestClosingTheActiveTerminalTabStartsWhicheverTabBecomesActiveIfUnstarted
// guards against a gap where closing the active terminal tab could leave
// focus on a terminal pane whose now-active tab was never started:
// keystrokes would then be silently swallowed by
// terminal.Model.handleKey's nil-pty guard until some unrelated
// focus-change event happened to trigger a start.
func TestClosingTheActiveTerminalTabStartsWhicheverTabBecomesActiveIfUnstarted(t *testing.T) {
	m := setupSizedApp(t)
	// cmdNewTerminalTab focuses+starts the new tab; tab 0 (the original)
	// is never focused here, so it stays unstarted.
	m, _ = cmdNewTerminalTab(m) // 2 tabs: [0 unstarted, 1 started+active], focus=focusTerminal
	t.Cleanup(func() {
		for i := range m.terminals {
			m.terminals[i].term.Close()
		}
	})
	if m.terminals[0].term.Started() {
		t.Fatal("test setup: expected tab 0 to still be unstarted before the close")
	}
	if m.focus != focusTerminal {
		t.Fatal("test setup: expected cmdNewTerminalTab to leave the terminal pane focused")
	}

	_, _, term := m.paneLayout()
	regions := terminalTabRegions(m.terminals, term.tabBar.x1-term.tabBar.x0)
	closeX := term.tabBar.x0 + regions[1].closeStart // close the active tab (1)
	closeY := term.tabBar.y0

	updated, _ := m.Update(tea.MouseMsg{X: closeX, Y: closeY, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = updated.(Model)

	if len(m.terminals) != 1 {
		t.Fatalf("got %d terminal tabs after closing tab 1, want 1", len(m.terminals))
	}
	if m.activeTerminal != 0 {
		t.Fatalf("got activeTerminal=%d, want 0 (the only tab left)", m.activeTerminal)
	}
	if !m.terminals[m.activeTerminal].term.Started() {
		t.Fatal("expected the tab that became active after the close to be started, since the terminal pane was already focused")
	}
}

func TestResizeAllPanesReachesEveryTerminalTabWithoutDisturbingItsRunningState(t *testing.T) {
	// terminal.Model exposes no getter for its stored width/height, and
	// internal/app cannot reach into internal/terminal's unexported
	// newPty/newEmulator test seams across the package boundary — so this
	// spawns two real shells (matching this codebase's existing
	// terminal-focused app tests, e.g. TestClickInTerminalPaneFocusesTerminal)
	// and checks the one thing observable from here: a resize that reaches
	// every tab (not just the active one, and not just index 0) must not
	// panic, lose track of a tab, or disturb a background tab's running
	// state — the same class of regression this codebase already fixed
	// once for boundary-drag resizes not reaching every open editor tab.
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	m, _ = cmdNewTerminalTab(m) // tab 1 active

	m.terminals[0].term, _ = m.terminals[0].term.Start() // background
	m.terminals[1].term, _ = m.terminals[1].term.Start() // active

	updated, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 35})
	m = updated.(Model)

	if len(m.terminals) != 2 {
		t.Fatalf("got %d terminal tabs after a resize, want 2 (none lost or duplicated)", len(m.terminals))
	}
	if !m.terminals[0].term.Started() || !m.terminals[1].term.Started() {
		t.Fatal("expected both tabs to remain started after a resize reaches every tab")
	}
	t.Cleanup(func() {
		m.terminals[0].term.Close()
		m.terminals[1].term.Close()
	})
}

// TestBeginResizeDragNoLongerTreatsTheTerminalTabBarsOwnRowAsABoundary
// replaces the old TestBeginResizeDragOnTheTerminalBoundaryStillWorksWithTheNewTabBarSubLayout
// (which asserted the opposite). Update's own mouse-press dispatch already
// intercepts any click landing anywhere in the terminal tab bar's full rect
// — the same x-range this checks — and routes it to handleClick before
// beginResizeDrag is ever tried (see the doc comment on that dispatch), so
// beginResizeDrag itself can no longer observe a call at the tab bar's own
// row through the real input path; the row it still recognizes as the
// editor/terminal boundary is exclusively the editor's own bottom border
// (already covered end-to-end by TestDraggingEditorTerminalBoundaryResizesTerminalHeight
// and TestDraggingEditorTerminalBoundaryClampsToMinAndMaxHeight).
func TestBeginResizeDragNoLongerTreatsTheTerminalTabBarsOwnRowAsABoundary(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)

	_, _, term := m.paneLayout()
	if _, ok := m.beginResizeDrag(m.treeWidth, term.tabBar.y0); ok {
		t.Fatal("expected beginResizeDrag to no longer treat the terminal tab bar's own row as a resize boundary — that row is exclusively tab-bar-click territory now, reached only through Update's own priority check before beginResizeDrag is ever tried")
	}
}

// TestWindowShrinkDoesNotPermanentlyShrinkThePanes: shrinking the window
// below what the panes need clamps the tree width and terminal height
// down to fit — but that clamp is a consequence of the window's size at
// that moment, not something the user asked for. Growing the window
// back must bring both panes back to the sizes they had, not leave the
// terminal stuck at its one-row minimum (with everything it was showing
// squeezed out of view) until someone drags it open again by hand.
func TestWindowShrinkDoesNotPermanentlyShrinkThePanes(t *testing.T) {
	m, err := New(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 150, Height: 45})
	m = updated.(Model)
	wantTree, wantTerm := m.treeWidth, m.terminalHeight

	for _, sz := range []tea.WindowSizeMsg{{Width: 60, Height: 15}, {Width: 20, Height: 6}, {Width: 2, Height: 2}, {Width: 20, Height: 6}, {Width: 150, Height: 45}} {
		updated, _ = m.Update(sz)
		m = updated.(Model)
	}
	if m.treeWidth != wantTree || m.terminalHeight != wantTerm {
		t.Fatalf("got treeWidth=%d terminalHeight=%d after shrinking the window and restoring it, want the original %d/%d back", m.treeWidth, m.terminalHeight, wantTree, wantTerm)
	}
}

// TestWindowShrinkKeepsADraggedPaneSizeAsThePreference: the size a pane
// returns to after a window shrink is the one the user last dragged it
// to, not the built-in default.
func TestWindowShrinkKeepsADraggedPaneSizeAsThePreference(t *testing.T) {
	m, err := New(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 150, Height: 45})
	m = updated.(Model)

	m.resizeDrag = resizeTerminal
	_, _, term := m.paneLayout()
	m = m.applyResizeDrag(80, term.terminal.y1-1-20) // drag the terminal open to 20 rows
	m.resizeDrag = resizeNone
	if m.terminalHeight != 20 {
		t.Fatalf("test setup: got terminalHeight=%d after the drag, want 20", m.terminalHeight)
	}

	for _, sz := range []tea.WindowSizeMsg{{Width: 40, Height: 10}, {Width: 150, Height: 45}} {
		updated, _ = m.Update(sz)
		m = updated.(Model)
	}
	if m.terminalHeight != 20 {
		t.Fatalf("got terminalHeight=%d after shrinking the window and restoring it, want the dragged 20 back", m.terminalHeight)
	}
}
