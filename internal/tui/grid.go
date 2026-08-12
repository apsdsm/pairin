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

	cellStyle CellStyle

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
	// Key identifies the cell uniquely across the whole grid. It matters in the
	// fleet view, where two projects may each have a service called "web";
	// within a single project, names are already unique and Key can be left
	// empty, in which case Name is used.
	Key string

	Name         string
	Color        string
	Status       process.Status
	Healthy      bool
	HasHealth    bool
	RestartCount int
	MaxRestarts  int
	PID          int
	DependsOn    []string
}

// key is the cell's identity for selection purposes.
func (c GridCell) key() string {
	if c.Key != "" {
		return c.Key
	}
	return c.Name
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

// CellStyle is how much room each service is given on screen.
type CellStyle int

const (
	// CellPlain: one line per service, no border. Fits the most on screen.
	CellPlain CellStyle = iota
	// CellBoxed: a box per service, one line of content.
	CellBoxed
	// CellCard: a box per service with a second line carrying its state.
	CellCard
)

func (s CellStyle) String() string {
	switch s {
	case CellBoxed:
		return "boxed"
	case CellCard:
		return "cards"
	default:
		return "plain"
	}
}

// Next cycles through the styles, densest first.
func (s CellStyle) Next() CellStyle {
	switch s {
	case CellPlain:
		return CellBoxed
	case CellBoxed:
		return CellCard
	default:
		return CellPlain
	}
}

// lineKind tags what a laid-out line holds.
type lineKind int

const (
	lineBlank lineKind = iota
	lineTitle
	lineNote
	lineRow
)

// lineSpec is one laid-out line of the grid. A lineRow may render to several
// screen lines when cells are boxed.
type lineSpec struct {
	kind  lineKind
	group int
	cells []int // flat cell indices, for lineRow
}

// gridLayout is the geometry of the grid: which cells sit in which visual row,
// and where each row lands on screen.
//
// Rendering and navigation both read this, which is the point. They used to
// compute geometry separately — rendering broke rows at every group boundary
// while navigation did flat arithmetic over a uniform column count — so moving
// down out of a short group skipped whole projects.
type gridLayout struct {
	cols     int
	cellWide int

	cells []GridCell // visible cells, flat, in render order
	specs []lineSpec

	// rows holds the flat cell indices of each visual row, in render order and
	// including row breaks at group boundaries. Vertical movement walks this.
	rows [][]int
	// rowOfCell maps a flat cell index to its row in rows.
	rowOfCell []int
	// lineOfCell maps a flat cell index to the last screen line of its row, so
	// scrolling to it brings the whole row (borders included) into view.
	lineOfCell []int
	// totalLines is how many screen lines the whole grid occupies.
	totalLines int
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

// SetGroups replaces the grid's contents, keeping the selection on the same
// cell where possible. Contents are rebuilt on every update, and in the fleet
// view whole projects appear and disappear as supervisors come and go — a
// selection tracked by position would wander under the user's cursor.
func (g *Grid) SetGroups(groups []GridGroup) {
	previous := g.SelectedKey()
	g.groups = groups

	if previous != "" {
		for i, c := range g.visibleCells() {
			if c.key() == previous {
				g.selected = i
				return
			}
		}
	}
	g.clampSelection()
}

// SetFilter narrows the grid to cells whose name contains q (case-insensitive).
func (g *Grid) SetFilter(q string) {
	g.filter = q
	g.selected = 0
	g.clampSelection()
}

func (g *Grid) Filter() string { return g.filter }

// CellStyle reports how cells are currently drawn.
func (g *Grid) CellStyle() CellStyle { return g.cellStyle }

// SetCellStyle changes how much room each service gets.
func (g *Grid) SetCellStyle(s CellStyle) { g.cellStyle = s }

// CycleCellStyle steps to the next cell style.
func (g *Grid) CycleCellStyle() { g.cellStyle = g.cellStyle.Next() }

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

// Selected returns the selected cell, and false if the grid is empty. Callers
// resolve it back to their own state — the grid doesn't know about panes,
// service lists or supervisors.
func (g *Grid) Selected() (GridCell, bool) {
	cells := g.visibleCells()
	if g.selected < 0 || g.selected >= len(cells) {
		return GridCell{}, false
	}
	return cells[g.selected], true
}

// SelectedName returns the display name of the selected service, or "".
func (g *Grid) SelectedName() string {
	c, ok := g.Selected()
	if !ok {
		return ""
	}
	return c.Name
}

// SelectedKey returns the identity of the selected cell, or "".
func (g *Grid) SelectedKey() string {
	c, ok := g.Selected()
	if !ok {
		return ""
	}
	return c.key()
}

// SelectKey moves the selection to a cell by identity, if it's visible.
func (g *Grid) SelectKey(key string) {
	for i, c := range g.visibleCells() {
		if c.key() == key {
			g.selected = i
			return
		}
	}
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

// Move shifts the selection by dx columns and dy rows.
//
// Horizontal movement runs along the flat cell order, so walking off the end of
// one project continues into the next. Vertical movement walks the *rendered*
// rows, which is what makes crossing a group boundary land where the eye
// expects: the row below the last row of a project is the first row of the next
// one, however many cells that project happened to have.
func (g *Grid) Move(dx, dy int) {
	l := g.layout()
	n := len(l.cells)
	if n == 0 {
		return
	}
	if g.selected >= n {
		g.selected = n - 1
	}
	if g.selected < 0 {
		g.selected = 0
	}

	if dx != 0 {
		next := g.selected + dx
		if next < 0 {
			next = 0
		}
		if next >= n {
			next = n - 1
		}
		g.selected = next
	}

	if dy == 0 || len(l.rows) == 0 {
		return
	}

	row := l.rowOfCell[g.selected]
	col := 0
	for i, idx := range l.rows[row] {
		if idx == g.selected {
			col = i
			break
		}
	}

	step := 1
	if dy < 0 {
		step = -1
	}
	for i := 0; i < abs(dy); i++ {
		next := row + step
		if next < 0 || next >= len(l.rows) {
			break
		}
		row = next
	}

	target := l.rows[row]
	if col >= len(target) {
		col = len(target) - 1
	}
	g.selected = target[col]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// layout computes the grid's geometry. Pure: it depends only on the groups, the
// filter and the size, so rendering and navigation always agree.
func (g Grid) layout() gridLayout {
	groups := g.visibleGroups()

	var labels []string
	for _, grp := range groups {
		for _, c := range grp.Cells {
			labels = append(labels, c.Name)
			// A card's second line can be wider than the service's name
			// ("pid 2994109" against "api"), so it has to be measured too or
			// it gets truncated in cells that otherwise have room to spare.
			if g.cellStyle == CellCard {
				labels = append(labels, cellDetail(c))
			}
		}
	}

	cols, cellWide := gridColumns(g.width, labels)
	if g.cellStyle != CellPlain {
		// Borders and the gutter between boxes need columns of their own.
		cellWide += 2
		cols = g.width / cellWide
		if cols < 1 {
			cols = 1
		}
	}

	l := gridLayout{cols: cols, cellWide: cellWide}
	showTitles := len(groups) > 1 || (len(groups) == 1 && groups[0].Title != "")
	line := 0
	rowHeight := g.rowHeight()

	for gi, grp := range groups {
		if showTitles {
			if gi > 0 {
				l.specs = append(l.specs, lineSpec{kind: lineBlank, group: gi})
				line++
			}
			l.specs = append(l.specs, lineSpec{kind: lineTitle, group: gi})
			line++
		}
		if grp.Note != "" && len(grp.Cells) == 0 {
			l.specs = append(l.specs, lineSpec{kind: lineNote, group: gi})
			line++
			continue
		}

		for start := 0; start < len(grp.Cells); start += cols {
			end := start + cols
			if end > len(grp.Cells) {
				end = len(grp.Cells)
			}
			spec := lineSpec{kind: lineRow, group: gi}
			var rowCells []int
			for i := start; i < end; i++ {
				flat := len(l.cells)
				l.cells = append(l.cells, grp.Cells[i])
				l.rowOfCell = append(l.rowOfCell, len(l.rows))
				// Point at the row's last screen line, so scrolling to the
				// selection brings the whole block into view.
				l.lineOfCell = append(l.lineOfCell, line+rowHeight-1)
				spec.cells = append(spec.cells, flat)
				rowCells = append(rowCells, flat)
			}
			l.specs = append(l.specs, spec)
			l.rows = append(l.rows, rowCells)
			line += rowHeight
		}
	}
	l.totalLines = line
	return l
}

// rowHeight is how many screen lines one row of cells occupies.
func (g Grid) rowHeight() int {
	switch g.cellStyle {
	case CellBoxed:
		return 3 // top border, content, bottom border
	case CellCard:
		return 4 // top border, name, detail, bottom border
	default:
		return 1
	}
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

// cellLabel is the glyph plus name, with the restart counter folded in.
func cellLabel(c GridCell, avail int) string {
	glyph, glyphStyle := statusGlyph(c)

	label := c.Name
	if c.Status == process.StatusRestarting && c.RestartCount > 0 {
		if c.MaxRestarts > 0 {
			label = fmt.Sprintf("%s %d/%d", label, c.RestartCount, c.MaxRestarts)
		} else {
			label = fmt.Sprintf("%s #%d", label, c.RestartCount)
		}
	}
	if avail > 0 && lipgloss.Width(label) > avail {
		label = truncate(label, avail)
	}

	nameStyle := lipgloss.NewStyle().Foreground(ServiceColor(c.Color))
	if c.Status == process.StatusStopped {
		nameStyle = nameStyle.Faint(true)
	}
	return glyphStyle.Render(glyph) + " " + nameStyle.Render(label)
}

// cellDetail is the second line of a card: what the glyph can't say on its own.
func cellDetail(c GridCell) string {
	switch c.Status {
	case process.StatusRunning:
		if c.HasHealth && !c.Healthy {
			return "unhealthy"
		}
		if c.PID > 0 {
			return fmt.Sprintf("pid %d", c.PID)
		}
		return "running"
	case process.StatusRestarting:
		if c.MaxRestarts > 0 {
			return fmt.Sprintf("retry %d/%d", c.RestartCount, c.MaxRestarts)
		}
		return fmt.Sprintf("retry #%d", c.RestartCount)
	case process.StatusWaiting:
		if len(c.DependsOn) > 0 {
			return "waits " + c.DependsOn[0]
		}
		return "waiting"
	default:
		return c.Status.String()
	}
}

// boxChars returns the border pieces for a cell. The selected cell gets a heavy
// border as well as the caret — the caret alone is easy to lose among twenty
// boxes, and a heavy edge reads at a glance without inverting a whole block.
func boxChars(selected bool) (tl, tr, bl, br, h, v string, style lipgloss.Style) {
	if selected {
		return "┏", "┓", "┗", "┛", "━", "┃", lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	}
	return "╭", "╮", "╰", "╯", "─", "│", lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
}

// renderBoxedRow draws one row of boxed cells, returning its screen lines.
// Boxes are separated by a gutter: butted together, adjacent borders read as a
// doubled rule heavier than the boxes themselves.
func renderBoxedRow(cells []GridCell, selectedAt int, cellWide int, card bool) []string {
	const gutter = 1
	boxWide := cellWide - gutter
	inner := boxWide - 2
	if inner < 3 {
		inner = 3
		boxWide = inner + 2
	}

	height := 3
	if card {
		height = 4
	}
	lines := make([]string, height)

	for i, c := range cells {
		selected := i == selectedAt
		tl, tr, bl, br, h, v, style := boxChars(selected)

		marker := " "
		if selected {
			marker = "›"
		}

		top := style.Render(tl + strings.Repeat(h, inner) + tr)
		bottom := style.Render(bl + strings.Repeat(h, inner) + br)
		name := style.Render(v) + padTo(marker+cellLabel(c, inner-3), inner) + style.Render(v)

		lines[0] += top + strings.Repeat(" ", gutter)
		lines[1] += name + strings.Repeat(" ", gutter)
		if card {
			detail := DimStyle.Render(truncate(cellDetail(c), inner-3))
			lines[2] += style.Render(v) + padTo("   "+detail, inner) + style.Render(v) + strings.Repeat(" ", gutter)
		}
		lines[height-1] += bottom + strings.Repeat(" ", gutter)
	}
	return lines
}

// padTo pads to a display width, ignoring the styling escapes inside s.
func padTo(s string, n int) string {
	if d := n - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
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
	l := g.layout()
	groups := g.visibleGroups()

	if len(l.specs) == 0 {
		if g.filter != "" {
			return DimStyle.Render(fmt.Sprintf("no services matching %q", g.filter))
		}
		return DimStyle.Render("no services")
	}

	var lines []string
	for _, spec := range l.specs {
		switch spec.kind {
		case lineBlank:
			lines = append(lines, "")

		case lineTitle:
			grp := groups[spec.group]
			title := HeaderStyle.Render(grp.Title)
			if grp.Subtitle != "" {
				title += "  " + DimStyle.Render(grp.Subtitle)
			}
			lines = append(lines, title)

		case lineNote:
			lines = append(lines, "  "+DimStyle.Render(groups[spec.group].Note))

		case lineRow:
			cells := make([]GridCell, len(spec.cells))
			selectedAt := -1
			for i, flat := range spec.cells {
				cells[i] = l.cells[flat]
				if flat == g.selected {
					selectedAt = i
				}
			}
			if g.cellStyle == CellPlain {
				var row strings.Builder
				for i, c := range cells {
					row.WriteString(renderCell(c, i == selectedAt, l.cellWide))
				}
				lines = append(lines, row.String())
				continue
			}
			lines = append(lines, renderBoxedRow(cells, selectedAt, l.cellWide, g.cellStyle == CellCard)...)
		}
	}

	return strings.Join(g.window(lines, l), "\n")
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
	if g.selected >= 0 && g.selected < len(layout.lineOfCell) {
		selRow = layout.lineOfCell[g.selected]
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
			PID:          v.PID,
			DependsOn:    v.DependsOn,
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
