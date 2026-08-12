package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/apsdsm/pairin/internal/process"
)

// Grid renders services as a compact wrapping grid of status cells, grouped by
// project. It is the answer to a config with twenty services, where giving each
// one a log viewport leaves every viewport too short to read.
//
// The same component backs the fleet dashboard, where each group is a separate
// project — which is why it carries group titles it doesn't strictly need when
// showing a single project.
type Grid struct {
	groups []GridGroup
	filter string

	width  int
	height int

	// selected is a flat index across every visible cell. Groups come and go
	// as projects start and stop, and flattening keeps navigation from having
	// to care about that.
	//
	// Everything else about the grid's appearance — column count, row
	// positions, scroll offset — is derived from this plus the size, never
	// stored. Bubble Tea hands View a copy of the model, so anything a render
	// computed and stashed would be thrown away before the next keystroke.
	selected int
}

// GridCell is one service.
type GridCell struct {
	Name         string
	Color        string
	Status       process.Status
	Healthy      bool
	HasHealth    bool
	RestartCount int
	MaxRestarts  int
}

// GridGroup is a titled set of cells — one project's worth.
type GridGroup struct {
	Title    string
	Subtitle string
	// Note replaces the cells when there's nothing running to show, e.g. a
	// registered project that hasn't been started.
	Note  string
	Cells []GridCell
}

// gridLayout is the computed geometry of one render pass.
type gridLayout struct {
	cols     int
	cellWide int
	// rowOfCell maps a flat cell index to the line it was rendered on, so
	// scrolling can keep the selection visible.
	rowOfCell []int
}

const (
	// gridMinCellWidth keeps short service names from producing a grid so wide
	// it reads as a list, and long ones from producing a single column.
	gridMinCellWidth = 14
	gridMaxCellWidth = 32
	gridCellGap      = 2
)

func NewGrid() Grid { return Grid{} }

func (g *Grid) SetSize(width, height int) {
	g.width = width
	g.height = height
}

// SetGroups replaces the grid's contents, clamping the selection so it stays
// on a real cell when services appear or disappear.
func (g *Grid) SetGroups(groups []GridGroup) {
	g.groups = groups
	g.clampSelection()
}

// SetFilter narrows the grid to cells whose name contains q (case-insensitive).
func (g *Grid) SetFilter(q string) {
	g.filter = q
	g.selected = 0
	g.clampSelection()
}

func (g *Grid) Filter() string { return g.filter }

// visibleGroups applies the filter. Groups that lose all their cells are kept
// only if they had a note to show in the first place — an empty project header
// is noise.
func (g *Grid) visibleGroups() []GridGroup {
	if g.filter == "" {
		return g.groups
	}
	q := strings.ToLower(g.filter)
	var out []GridGroup
	for _, grp := range g.groups {
		kept := GridGroup{Title: grp.Title, Subtitle: grp.Subtitle}
		for _, c := range grp.Cells {
			if strings.Contains(strings.ToLower(c.Name), q) {
				kept.Cells = append(kept.Cells, c)
			}
		}
		if len(kept.Cells) > 0 {
			out = append(out, kept)
		}
	}
	return out
}

// visibleCells flattens the filtered groups in render order.
func (g *Grid) visibleCells() []GridCell {
	var out []GridCell
	for _, grp := range g.visibleGroups() {
		out = append(out, grp.Cells...)
	}
	return out
}

// SelectedName returns the name of the selected service, or "" if the grid is
// empty. Callers resolve the name back to their own index — the grid doesn't
// know about panes or service lists.
func (g *Grid) SelectedName() string {
	cells := g.visibleCells()
	if g.selected < 0 || g.selected >= len(cells) {
		return ""
	}
	return cells[g.selected].Name
}

// SelectName moves the selection to a named service if it's visible.
func (g *Grid) SelectName(name string) {
	for i, c := range g.visibleCells() {
		if c.Name == name {
			g.selected = i
			return
		}
	}
}

func (g *Grid) clampSelection() {
	n := len(g.visibleCells())
	if n == 0 {
		g.selected = 0
		return
	}
	if g.selected >= n {
		g.selected = n - 1
	}
	if g.selected < 0 {
		g.selected = 0
	}
}

// Move shifts the selection by dx columns and dy rows. Movement is over the
// flat cell order, so running off the end of one group lands in the next.
func (g *Grid) Move(dx, dy int) {
	cells := g.visibleCells()
	n := len(cells)
	if n == 0 {
		return
	}

	names := make([]string, len(cells))
	for i, c := range cells {
		names[i] = c.Name
	}
	cols, _ := gridColumns(g.width, names)

	next := g.selected + dx + dy*cols
	if next < 0 {
		next = 0
	}
	if next >= n {
		next = n - 1
	}
	g.selected = next
}

// gridColumns computes how many cells fit across the given width, and how wide
// each one is. Split out from rendering so the geometry can be tested directly.
func gridColumns(width int, labels []string) (cols, cellWide int) {
	longest := 0
	for _, l := range labels {
		if w := lipgloss.Width(l); w > longest {
			longest = w
		}
	}

	// 2 leading columns for the status glyph and its space.
	cellWide = longest + 2 + gridCellGap
	if cellWide < gridMinCellWidth {
		cellWide = gridMinCellWidth
	}
	if cellWide > gridMaxCellWidth {
		cellWide = gridMaxCellWidth
	}

	cols = width / cellWide
	if cols < 1 {
		cols = 1
	}
	return cols, cellWide
}

// statusGlyph maps a service's state to a single character and its style.
// Health is orthogonal to status: a service can be running but not yet healthy,
// and the grid is the one place that distinction has to survive being compressed
// into one character.
func statusGlyph(c GridCell) (string, lipgloss.Style) {
	switch c.Status {
	case process.StatusRunning:
		if c.HasHealth && !c.Healthy {
			return "◍", StatusUnhealthy
		}
		return "●", StatusRunning
	case process.StatusStarting:
		return "◐", StatusStarting
	case process.StatusWaiting:
		return "⋯", StatusWaitingStyle
	case process.StatusRestarting:
		return "⟳", StatusRestarting
	case process.StatusCrashed:
		return "✕", StatusCrashed
	default:
		return "○", StatusStopped
	}
}

// GridLegend is the key to the glyphs, rendered under the grid.
func GridLegend() string {
	parts := []string{
		StatusRunning.Render("●") + DimStyle.Render(" up"),
		StatusUnhealthy.Render("◍") + DimStyle.Render(" unhealthy"),
		StatusStarting.Render("◐") + DimStyle.Render(" starting"),
		StatusWaitingStyle.Render("⋯") + DimStyle.Render(" waiting"),
		StatusRestarting.Render("⟳") + DimStyle.Render(" restarting"),
		StatusCrashed.Render("✕") + DimStyle.Render(" crashed"),
		StatusStopped.Render("○") + DimStyle.Render(" stopped"),
	}
	return strings.Join(parts, "  ")
}

// renderCell draws one cell, padded to cellWide. The selected cell is marked
// with a caret rather than reverse video: at twenty-plus cells a block of
// inverted colour fights with the status colours for attention.
func renderCell(c GridCell, selected bool, cellWide int) string {
	glyph, glyphStyle := statusGlyph(c)

	label := c.Name
	if c.Status == process.StatusRestarting && c.RestartCount > 0 {
		if c.MaxRestarts > 0 {
			label = fmt.Sprintf("%s %d/%d", label, c.RestartCount, c.MaxRestarts)
		} else {
			label = fmt.Sprintf("%s #%d", label, c.RestartCount)
		}
	}

	// Room for the glyph, its space, and the selection caret.
	avail := cellWide - gridCellGap - 2
	if avail < 1 {
		avail = 1
	}
	if lipgloss.Width(label) > avail {
		label = truncate(label, avail)
	}

	nameStyle := lipgloss.NewStyle().Foreground(ServiceColor(c.Color))
	if selected {
		nameStyle = nameStyle.Bold(true).Underline(true)
	} else if c.Status == process.StatusStopped {
		nameStyle = nameStyle.Faint(true)
	}

	marker := " "
	if selected {
		marker = "›"
	}

	out := marker + glyphStyle.Render(glyph) + " " + nameStyle.Render(label)
	pad := cellWide - lipgloss.Width(out)
	if pad > 0 {
		out += strings.Repeat(" ", pad)
	}
	return out
}

func truncate(s string, max int) string {
	if max <= 1 {
		return "…"
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

// View renders the grid, windowed to its height with the selection kept in
// sight. It is deliberately non-mutating — see the note on Grid.selected.
func (g Grid) View() string {
	groups := g.visibleGroups()

	var labels []string
	for _, grp := range groups {
		for _, c := range grp.Cells {
			labels = append(labels, c.Name)
		}
	}

	cols, cellWide := gridColumns(g.width, labels)
	layout := gridLayout{cols: cols, cellWide: cellWide}
	layout.rowOfCell = make([]int, 0, len(labels))

	var lines []string
	showTitles := len(groups) > 1 || (len(groups) == 1 && groups[0].Title != "")

	for gi, grp := range groups {
		if showTitles {
			if gi > 0 {
				lines = append(lines, "")
			}
			title := HeaderStyle.Render(grp.Title)
			if grp.Subtitle != "" {
				title += "  " + DimStyle.Render(grp.Subtitle)
			}
			lines = append(lines, title)
		}
		if grp.Note != "" && len(grp.Cells) == 0 {
			lines = append(lines, "  "+DimStyle.Render(grp.Note))
			continue
		}

		for start := 0; start < len(grp.Cells); start += cols {
			end := start + cols
			if end > len(grp.Cells) {
				end = len(grp.Cells)
			}
			var row strings.Builder
			for i := start; i < end; i++ {
				flat := len(layout.rowOfCell)
				layout.rowOfCell = append(layout.rowOfCell, len(lines))
				row.WriteString(renderCell(grp.Cells[i], flat == g.selected, cellWide))
			}
			lines = append(lines, row.String())
		}
	}

	if len(lines) == 0 {
		if g.filter != "" {
			return DimStyle.Render(fmt.Sprintf("no services matching %q", g.filter))
		}
		return DimStyle.Render("no services")
	}

	return strings.Join(g.window(lines, layout), "\n")
}

// window trims the rendered lines to the grid's height, keeping the selected
// cell on screen. The offset is derived from the selection rather than carried
// between renders: the selection sits on the last visible row while you move
// down past the fold, and the view follows one line at a time on the way back
// up. Stateless, and predictable in both directions.
func (g Grid) window(lines []string, layout gridLayout) []string {
	if g.height <= 0 || len(lines) <= g.height {
		return lines
	}

	selRow := 0
	if g.selected >= 0 && g.selected < len(layout.rowOfCell) {
		selRow = layout.rowOfCell[g.selected]
	}

	scroll := 0
	if selRow >= g.height {
		scroll = selRow - g.height + 1
	}
	if max := len(lines) - g.height; scroll > max {
		scroll = max
	}

	return lines[scroll : scroll+g.height]
}

// GridCellsFor builds cells from a service list.
func GridCellsFor(svcs []*process.Service) []GridCell {
	cells := make([]GridCell, 0, len(svcs))
	for _, svc := range svcs {
		v := svc.View()
		cells = append(cells, GridCell{
			Name:         v.Name,
			Color:        v.Color,
			Status:       v.Status,
			Healthy:      v.Healthy,
			HasHealth:    v.HasHealth,
			RestartCount: v.RestartCount,
			MaxRestarts:  v.MaxRestarts,
		})
	}
	return cells
}

// SummarizeCells counts how many services are up, for a header line.
func SummarizeCells(cells []GridCell) (up, total int) {
	for _, c := range cells {
		if c.Status == process.StatusRunning {
			up++
		}
	}
	return up, len(cells)
}
