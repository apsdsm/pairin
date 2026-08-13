package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/apsdsm/pairin/internal/process"
	"github.com/apsdsm/pairin/internal/state"
)

func cells(names ...string) []GridCell {
	out := make([]GridCell, len(names))
	for i, n := range names {
		out[i] = GridCell{Name: n, Status: process.StatusRunning}
	}
	return out
}

func gridOf(width, height int, names ...string) Grid {
	g := NewGrid()
	g.SetSize(width, height)
	g.SetGroups([]GridGroup{{Cells: cells(names...)}})
	return g
}

func TestGridColumnsFitWidth(t *testing.T) {
	tests := []struct {
		name     string
		width    int
		labels   []string
		wantCols int
	}{
		{"short names pack in", 80, []string{"api", "web", "db"}, 5},          // min cell width 14
		{"long names widen cells", 80, []string{strings.Repeat("x", 24)}, 2},  // 24+4 = 28
		{"absurd names are capped", 80, []string{strings.Repeat("x", 60)}, 2}, // capped at 32
		{"narrow terminal still gets one", 10, []string{"api"}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cols, cellWide := gridColumns(tt.width, tt.labels)
			if cols != tt.wantCols {
				t.Errorf("cols = %d, want %d (cell width %d)", cols, tt.wantCols, cellWide)
			}
			if cols > 1 && cols*cellWide > tt.width {
				t.Errorf("%d columns of %d overflow width %d", cols, cellWide, tt.width)
			}
		})
	}
}

func TestGridMoveStaysInBounds(t *testing.T) {
	g := gridOf(80, 20, "a", "b", "c")

	g.Move(-1, 0)
	if got := g.SelectedName(); got != "a" {
		t.Errorf("moving left from the first cell landed on %q, want %q", got, "a")
	}

	g.Move(99, 0)
	if got := g.SelectedName(); got != "c" {
		t.Errorf("moving past the end landed on %q, want %q", got, "c")
	}

	g.Move(0, 99)
	if got := g.SelectedName(); got != "c" {
		t.Errorf("moving down past the end landed on %q, want %q", got, "c")
	}
}

func TestGridMoveDownCrossesARow(t *testing.T) {
	// 80 wide with short names gives 5 columns, so cell 0 + one row = cell 5.
	g := gridOf(80, 20, "a", "b", "c", "d", "e", "f", "g")

	g.Move(0, 1)
	if got := g.SelectedName(); got != "f" {
		t.Errorf("down from %q landed on %q, want %q", "a", got, "f")
	}
	g.Move(0, -1)
	if got := g.SelectedName(); got != "a" {
		t.Errorf("up again landed on %q, want %q", got, "a")
	}
}

func TestGridFilterNarrowsAndResets(t *testing.T) {
	g := gridOf(80, 20, "postgres", "redis", "api", "api-worker")

	g.SetFilter("api")
	if got := len(g.visibleCells()); got != 2 {
		t.Fatalf("filter matched %d cells, want 2", got)
	}
	if got := g.SelectedName(); got != "api" {
		t.Errorf("selection after filtering = %q, want %q", got, "api")
	}

	// Filtering is case-insensitive.
	g.SetFilter("REDIS")
	if got := g.SelectedName(); got != "redis" {
		t.Errorf("case-insensitive filter selected %q, want %q", got, "redis")
	}

	g.SetFilter("")
	if got := len(g.visibleCells()); got != 4 {
		t.Errorf("clearing the filter left %d cells, want 4", got)
	}
}

// A filter that matches nothing must not leave the selection pointing at a cell
// that no longer exists.
func TestGridFilterMatchingNothing(t *testing.T) {
	g := gridOf(80, 20, "api", "web")
	g.Move(1, 0)

	g.SetFilter("nothing-matches-this")
	if got := g.SelectedName(); got != "" {
		t.Errorf("selection with no matches = %q, want empty", got)
	}
	if view := g.View(); !strings.Contains(view, "no services matching") {
		t.Errorf("view did not explain the empty result:\n%s", view)
	}
}

// Services disappearing (a config with fewer entries after a reconnect) must
// not leave the selection out of range.
func TestGridSelectionClampsWhenCellsVanish(t *testing.T) {
	g := gridOf(80, 20, "a", "b", "c")
	g.Move(2, 0)
	if got := g.SelectedName(); got != "c" {
		t.Fatalf("setup: selection = %q, want %q", got, "c")
	}

	g.SetGroups([]GridGroup{{Cells: cells("a")}})
	if got := g.SelectedName(); got != "a" {
		t.Errorf("selection after shrinking = %q, want %q", got, "a")
	}
}

func TestGridWindowKeepsSelectionVisible(t *testing.T) {
	// 20 cells at 5 columns = 4 rows, windowed to 2.
	names := []string{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j",
		"k", "l", "m", "n", "o", "p", "q", "r", "s", "t"} {
		names = append(names, n)
	}
	g := gridOf(80, 2, names...)

	// Selecting the last cell must scroll it into the window.
	g.Move(19, 0)
	view := g.View()
	if lines := strings.Count(view, "\n") + 1; lines != 2 {
		t.Errorf("view rendered %d lines, want 2", lines)
	}
	if !strings.Contains(view, "›") {
		t.Errorf("selection marker scrolled out of view:\n%s", view)
	}
}

func TestGridViewDoesNotMutate(t *testing.T) {
	// Bubble Tea renders from a copy of the model, so View must not depend on
	// storing anything. Rendering twice must give the same result.
	g := gridOf(80, 5, "a", "b", "c")
	first := g.View()
	second := g.View()
	if first != second {
		t.Errorf("View is not idempotent:\n%q\n%q", first, second)
	}
}

func TestStatusGlyphDistinguishesUnhealthy(t *testing.T) {
	running := GridCell{Status: process.StatusRunning}
	if got, _ := statusGlyph(running); got != "●" {
		t.Errorf("running glyph = %q, want ●", got)
	}

	// Running but failing its healthcheck must not look identical to healthy.
	unhealthy := GridCell{Status: process.StatusRunning, HasHealth: true, Healthy: false}
	if got, _ := statusGlyph(unhealthy); got != "◍" {
		t.Errorf("unhealthy glyph = %q, want ◍", got)
	}

	healthy := GridCell{Status: process.StatusRunning, HasHealth: true, Healthy: true}
	if got, _ := statusGlyph(healthy); got != "●" {
		t.Errorf("healthy glyph = %q, want ●", got)
	}
}

func TestRenderCellPadsToWidth(t *testing.T) {
	// Cells must be exactly cellWide so columns line up regardless of name length.
	for _, name := range []string{"a", "medium-name", strings.Repeat("x", 50)} {
		out := renderCell(GridCell{Name: name, Status: process.StatusRunning}, false, 20)
		if got := lipglossWidth(out); got != 20 {
			t.Errorf("cell for %q rendered %d wide, want 20", name, got)
		}
	}
}

// lipglossWidth is a thin alias so the test reads without an extra import at
// the call sites.
func lipglossWidth(s string) int { return lipgloss.Width(s) }

// Vertical movement used to do flat arithmetic over a uniform column count
// while rendering broke rows at every group boundary. Moving down out of a
// short group therefore jumped by a whole row's worth of cells and skipped
// past entire projects. Both now read the same layout.
func TestGridMoveDownCrossesGroupBoundary(t *testing.T) {
	g := NewGrid()
	g.SetSize(80, 40) // 5 columns
	g.SetGroups([]GridGroup{
		{Title: "alpha", Cells: []GridCell{
			{Key: "a/one", Name: "one", Status: process.StatusRunning},
			{Key: "a/two", Name: "two", Status: process.StatusRunning},
		}},
		{Title: "beta", Cells: []GridCell{
			{Key: "b/three", Name: "three", Status: process.StatusRunning},
			{Key: "b/four", Name: "four", Status: process.StatusRunning},
		}},
		{Title: "gamma", Cells: []GridCell{
			{Key: "c/five", Name: "five", Status: process.StatusRunning},
		}},
	})

	// alpha has 2 cells in a 5-wide grid, so its single row is the whole group.
	// Down from "one" must land on "three" — the first cell of the next group —
	// not five cells further along the flat list.
	if got := g.SelectedKey(); got != "a/one" {
		t.Fatalf("setup: selection = %q, want a/one", got)
	}

	g.Move(0, 1)
	if got := g.SelectedKey(); got != "b/three" {
		t.Errorf("down from alpha landed on %q, want b/three", got)
	}

	g.Move(0, 1)
	if got := g.SelectedKey(); got != "c/five" {
		t.Errorf("down from beta landed on %q, want c/five", got)
	}

	// And back up again, symmetrically.
	g.Move(0, -1)
	if got := g.SelectedKey(); got != "b/three" {
		t.Errorf("up from gamma landed on %q, want b/three", got)
	}
	g.Move(0, -1)
	if got := g.SelectedKey(); got != "a/one" {
		t.Errorf("up from beta landed on %q, want a/one", got)
	}
}

// Moving down into a shorter row must clamp to that row's last cell rather
// than overshoot into the row after it.
func TestGridMoveDownClampsToShorterRow(t *testing.T) {
	g := NewGrid()
	g.SetSize(80, 40)
	g.SetGroups([]GridGroup{
		{Title: "alpha", Cells: []GridCell{
			{Key: "a/1", Name: "one"}, {Key: "a/2", Name: "two"}, {Key: "a/3", Name: "three"},
		}},
		{Title: "beta", Cells: []GridCell{
			{Key: "b/1", Name: "solo"},
		}},
	})

	g.Move(2, 0) // third cell of alpha
	if got := g.SelectedKey(); got != "a/3" {
		t.Fatalf("setup: selection = %q, want a/3", got)
	}
	g.Move(0, 1)
	if got := g.SelectedKey(); got != "b/1" {
		t.Errorf("down from column 3 into a one-cell group landed on %q, want b/1", got)
	}
}

// Vertical movement must not depend on the cell style: boxed rows are three
// screen lines tall, but they are still one row.
func TestGridMoveIsStyleIndependent(t *testing.T) {
	build := func(style CellStyle) Grid {
		g := NewGrid()
		g.SetSize(80, 40)
		g.SetCellStyle(style)
		g.SetGroups([]GridGroup{
			{Title: "alpha", Cells: []GridCell{{Key: "a/1", Name: "one"}, {Key: "a/2", Name: "two"}}},
			{Title: "beta", Cells: []GridCell{{Key: "b/1", Name: "three"}}},
		})
		return g
	}

	for _, style := range []CellStyle{CellPlain, CellBoxed, CellCard} {
		g := build(style)
		g.Move(0, 1)
		if got := g.SelectedKey(); got != "b/1" {
			t.Errorf("%s: down landed on %q, want b/1", style, got)
		}
	}
}

// Each style must still fit the width it was given.
func TestGridStylesFitWidth(t *testing.T) {
	for _, style := range []CellStyle{CellPlain, CellBoxed, CellCard} {
		g := NewGrid()
		g.SetSize(80, 40)
		g.SetCellStyle(style)
		g.SetGroups([]GridGroup{{Title: "alpha", Cells: cells("postgres", "redis", "api", "worker", "web")}})

		for i, line := range strings.Split(g.View(), "\n") {
			if w := lipglossWidth(line); w > 80 {
				t.Errorf("%s: line %d is %d wide, want at most 80: %q", style, i, w, line)
			}
		}
	}
}

// The cell style is remembered across sessions: cycling with 'b' writes the
// choice, and a fresh grid picks it up.
func TestCellStyleIsRemembered(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Nothing stored yet: the default is the densest style, which always fits.
	if got := RememberedCellStyle(); got != CellPlain {
		t.Errorf("default style = %s, want plain", got)
	}

	g := gridOf(80, 20, "a", "b")
	g.CycleCellStyle() // plain -> boxed
	if got := RememberedCellStyle(); got != CellBoxed {
		t.Errorf("after one cycle, remembered = %s, want boxed", got)
	}

	g.CycleCellStyle() // boxed -> cards
	if got := RememberedCellStyle(); got != CellCard {
		t.Errorf("after two cycles, remembered = %s, want cards", got)
	}

	// A grid built now opens in the style last used.
	fresh := NewGrid()
	fresh.SetCellStyle(RememberedCellStyle())
	if got := fresh.CellStyle(); got != CellCard {
		t.Errorf("fresh grid opened in %s, want cards", got)
	}

	// And cycling all the way round stores plain again rather than leaving a
	// stale value behind.
	g.CycleCellStyle() // cards -> plain
	if got := RememberedCellStyle(); got != CellPlain {
		t.Errorf("after three cycles, remembered = %s, want plain", got)
	}
}

// A missing or unreadable preferences file must never stop a TUI from opening.
func TestRememberedCellStyleToleratesGarbage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	path, err := state.UIPath()
	if err != nil {
		t.Fatalf("UIPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("this is not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := RememberedCellStyle(); got != CellPlain {
		t.Errorf("style from a corrupt file = %s, want plain", got)
	}

	// An unknown style name falls back too, rather than rendering nothing.
	if err := os.WriteFile(path, []byte(`{"cell_style":"hexagons"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := RememberedCellStyle(); got != CellPlain {
		t.Errorf("style from an unknown name = %s, want plain", got)
	}
}
