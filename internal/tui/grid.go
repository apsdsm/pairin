package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/apsdsm/pairin/internal/process"
	"github.com/apsdsm/pairin/internal/state"
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

	// Ports the service is listening on, discovered from the kernel. In card
	// style these are listed under the name, one per line.
	Ports []process.Port
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
	// Marker is drawn before the title, already styled by the caller. The grid
	// doesn't know what it means — it only reserves the room.
	Marker   string
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

// ParseCellStyle turns a stored name back into a style. Anything unrecognized
// falls back to plain, which always fits.
func ParseCellStyle(s string) CellStyle {
	switch s {
	case "boxed":
		return CellBoxed
	case "cards":
		return CellCard
	default:
		return CellPlain
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
	kind   lineKind
	group  int
	cells  []int // flat cell indices, for lineRow
	height int   // screen lines this row occupies, for lineRow
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

	// labelWide is the width port labels are padded to, so the colons line up
	// down the whole grid rather than only within a card.
	labelWide int

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

// CycleCellStyle steps to the next cell style and remembers the choice, so the
// next TUI opens in the style you were last using.
func (g *Grid) CycleCellStyle() {
	g.cellStyle = g.cellStyle.Next()
	rememberCellStyle(g.cellStyle)
}

// rememberCellStyle persists the choice. Best-effort: a preferences file that
// can't be written is not a reason to interrupt anyone.
func rememberCellStyle(s CellStyle) {
	ui := state.LoadUI()
	ui.CellStyle = s.String()
	_ = state.SaveUI(ui)
}

// RememberedCellStyle is the style the user was last in.
func RememberedCellStyle() CellStyle {
	return ParseCellStyle(state.LoadUI().CellStyle)
}

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

	// Measured before the details themselves, since it's what they pad to.
	labelWide := portLabelWidth(groups)

	var labels []string
	for _, grp := range groups {
		for _, c := range grp.Cells {
			labels = append(labels, c.Name)
			// A card's detail lines can be wider than the service's name
			// ("pid 2994109" against "api"), so they have to be measured too or
			// they get truncated in cells that otherwise have room to spare.
			if g.cellStyle == CellCard {
				labels = append(labels, cardDetails(c, maxPortLines, labelWide)...)
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

	l := gridLayout{cols: cols, cellWide: cellWide, labelWide: labelWide}
	showTitles := len(groups) > 1 || (len(groups) == 1 && groups[0].Title != "")
	line := 0
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
			// Height is per row, not per grid: a row containing a service with
			// three ports is taller than one where every service has a single
			// port, and each cell in it pads to match.
			rowHeight := g.rowHeight(grp.Cells[start:end])

			spec := lineSpec{kind: lineRow, group: gi, height: rowHeight}
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

// maxPortLines caps how many ports one card lists. A service can legitimately
// listen on a dozen ports, and one of those must not be allowed to set the
// height of every row on screen.
const maxPortLines = 4

// cardDetailLines is how many detail lines a card needs for one cell: its
// ports, one per line, or a single status line when it has none.
func cardDetailLines(c GridCell) int {
	n := len(c.Ports)
	if n == 0 {
		return 1
	}
	if n > maxPortLines {
		// The last line becomes "+N more", so the cap is the total.
		return maxPortLines
	}
	return n
}

// rowHeight is how many screen lines a row of cells occupies. Rows are ragged
// in card style — a service listening on three ports needs three lines — so
// every cell in a row is padded to the tallest, keeping the grid on a grid.
func (g Grid) rowHeight(cells []GridCell) int {
	switch g.cellStyle {
	case CellBoxed:
		return 3 // top border, content, bottom border
	case CellCard:
		detail := 1
		for _, c := range cells {
			if n := cardDetailLines(c); n > detail {
				detail = n
			}
		}
		return 3 + detail // top border, name, detail lines, bottom border
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

// Pin markers, drawn at the start of a project's heading in the fleet view.
const (
	GlyphPinned   = "◆"
	GlyphUnpinned = "◇"
)

// PinMarker returns the styled marker for a project's pin state.
func PinMarker(pinned bool) string {
	if pinned {
		return PinnedStyle.Render(GlyphPinned)
	}
	return UnpinnedStyle.Render(GlyphUnpinned)
}

// legendItem is one entry in the key.
type legendItem struct {
	rendered string
	width    int
}

func legend(glyph string, style lipgloss.Style, label string) legendItem {
	r := style.Render(glyph) + DimStyle.Render(" "+label)
	return legendItem{rendered: r, width: lipgloss.Width(r)}
}

func statusLegend() []legendItem {
	return []legendItem{
		legend("●", StatusRunning, "up"),
		legend("◍", StatusUnhealthy, "unhealthy"),
		legend("◐", StatusStarting, "starting"),
		legend("⋯", StatusWaitingStyle, "waiting"),
		legend("⟳", StatusRestarting, "restarting"),
		legend("✕", StatusCrashed, "crashed"),
		legend("○", StatusStopped, "stopped"),
	}
}

// renderLegend joins as many entries as fit the width, dropping from the end.
// Truncating mid-string would cut through the styling escapes; dropping whole
// entries keeps every one that is shown readable.
func renderLegend(items []legendItem, width int) string {
	const gap = 2

	var out []string
	used := 0
	for i, it := range items {
		add := it.width
		if i > 0 {
			add += gap
		}
		if width > 0 && used+add > width {
			break
		}
		used += add
		out = append(out, it.rendered)
	}
	return strings.Join(out, strings.Repeat(" ", gap))
}

// hints renders a row of key hints, dropping trailing ones that don't fit.
// The list grows every time a key is added, so pass them in descending order of
// how much the reader needs them.
func hints(width int, parts ...string) string {
	items := make([]legendItem, 0, len(parts))
	for _, p := range parts {
		r := FooterStyle.Render(p)
		items = append(items, legendItem{rendered: r, width: lipgloss.Width(r)})
	}
	return renderLegend(items, width)
}

// GridLegend is the key to the service glyphs, trimmed to the given width.
// A width of zero means no limit.
func GridLegend(width int) string {
	return renderLegend(statusLegend(), width)
}

// FleetLegend adds the project-level pin markers to the service key.
//
// They get a wider gap rather than a divider character: the markers describe
// projects while the glyphs before them describe services, and the break needs
// to be visible — but a divider costs three columns, which was enough to push
// the last entry off a 100-column terminal.
func FleetLegend(width int) string {
	pinned := legend(GlyphPinned, PinnedStyle, "pinned")
	pinned.rendered = " " + pinned.rendered
	pinned.width++

	items := append(statusLegend(), pinned, legend(GlyphUnpinned, UnpinnedStyle, "unpinned"))
	return renderLegend(items, width)
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
func renderBoxedRow(cells []GridCell, selectedAt int, cellWide int, card bool, height, labelWide int) []string {
	const gutter = 1
	boxWide := cellWide - gutter
	inner := boxWide - 2
	if inner < 3 {
		inner = 3
		boxWide = inner + 2
	}

	if height < 3 {
		height = 3
	}
	if !card {
		height = 3
	}
	detailLines := height - 3

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
			// Cells shorter than the row pad with blank interior, so the row
			// stays rectangular however ragged the ports are.
			details := cardDetails(c, detailLines, labelWide)
			for d := 0; d < detailLines; d++ {
				text := ""
				if d < len(details) {
					text = "   " + DimStyle.Render(truncate(details[d], inner-3))
				}
				lines[2+d] += style.Render(v) + padTo(text, inner) + style.Render(v) + strings.Repeat(" ", gutter)
			}
		}
		lines[height-1] += bottom + strings.Repeat(" ", gutter)
	}
	return lines
}

// cardDetails is what goes under a card's name: the ports it is listening on,
// one per line. The last line absorbs any overflow so a service with a dozen
// ports doesn't stretch the row.
//
// Nothing when there are no ports. The slot means one thing — where to reach
// this service — and filling it with a PID when that answer isn't available
// puts two unrelated kinds of value in the same place, which reads as noise.
// The glyph already carries the status, and the PID is in the zoomed view.
//
// labelWide pads the labels so the colons line up; see portLabelWidth.
func cardDetails(c GridCell, room, labelWide int) []string {
	shown := shownPorts(c, room)
	if len(shown) == 0 {
		return nil
	}

	out := make([]string, 0, len(shown)+1)
	for _, p := range shown {
		out = append(out, portLabel(p, labelWide))
	}
	if n := len(c.Ports) - len(shown); n > 0 {
		// Deliberately unpadded: it isn't a port, so lining it up with them
		// would imply it was one.
		out = append(out, fmt.Sprintf("+%d more", n))
	}
	return out
}

// shownPorts is the ports a card actually lists, given the lines it has room
// for. When they don't all fit, the last line goes to "+N more" instead.
//
// Split out because the width the labels pad to has to be measured from the
// ports that are actually rendered — measuring hidden ones would indent every
// card on screen to accommodate a label nobody can see.
func shownPorts(c GridCell, room int) []process.Port {
	if room < 1 {
		room = 1
	}
	if len(c.Ports) <= room {
		return c.Ports
	}
	return c.Ports[:room-1]
}

// portLabelWidth is the width every port label is padded to, so that the
// colons form one vertical line down the grid rather than one per card.
// Aligning per card would do nothing for the common case — most services list a
// single port, and what doesn't line up is the card above and the card below.
//
// Measured across the whole grid because every column is the same width, so a
// single figure aligns the rows as well as the columns. Zero when no port on
// screen has a label, which leaves bare ports flush against the border instead
// of indenting them past a margin nothing occupies.
func portLabelWidth(groups []GridGroup) int {
	w := 0
	for _, grp := range groups {
		for _, c := range grp.Cells {
			for _, p := range shownPorts(c, maxPortLines) {
				if n := lipgloss.Width(truncate(p.Label, maxPortLabel)); n > w {
					w = n
				}
			}
		}
	}
	return w
}

// maxPortLabel bounds the label shown beside a port. Column width is sized to
// fit the widest detail, so an unbounded label would widen every cell in the
// grid — the same trap "waits <dependency>" fell into. Eight fits the names
// worth writing ("postgres", "frontend") and still bounds a detail line at
// 8 + " :" + 5 digits = 15.
const maxPortLabel = 8

// portLabel renders one port, with its label when it has one: "db :5432".
//
// labelWide right-pads the label so the colon lands in the same column on every
// line; pass 0 to render it flush, which is what a single port on its own line
// wants.
func portLabel(p process.Port, labelWide int) string {
	label := truncate(p.Label, maxPortLabel)
	if pad := labelWide - lipgloss.Width(label); pad > 0 {
		label += strings.Repeat(" ", pad)
	}
	if label == "" {
		return fmt.Sprintf(":%d", p.Number)
	}
	return fmt.Sprintf("%s :%d", label, p.Number)
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
			lines = append(lines, g.renderTitle(groups[spec.group]))

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
			lines = append(lines, renderBoxedRow(cells, selectedAt, l.cellWide, g.cellStyle == CellCard, spec.height, l.labelWide)...)
		}
	}

	return strings.Join(g.window(lines, l), "\n")
}

// renderTitle draws a group's heading, trimmed to the grid's width. Project
// paths and status notes are both open-ended, so the line has to be clipped
// rather than trusted to fit.
func (g Grid) renderTitle(grp GridGroup) string {
	prefix := ""
	prefixWidth := 0
	if grp.Marker != "" {
		prefix = grp.Marker + " "
		prefixWidth = lipgloss.Width(prefix)
	}

	avail := g.width - prefixWidth
	name := grp.Title
	if avail > 0 && lipgloss.Width(name) > avail {
		name = truncate(name, avail)
	}
	out := prefix + HeaderStyle.Render(name)

	if grp.Subtitle == "" {
		return out
	}
	room := g.width - prefixWidth - lipgloss.Width(name) - 2
	if g.width <= 0 {
		room = lipgloss.Width(grp.Subtitle)
	}
	if room < 4 {
		return out
	}
	sub := grp.Subtitle
	if lipgloss.Width(sub) > room {
		sub = truncate(sub, room)
	}
	return out + "  " + DimStyle.Render(sub)
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
			Ports:        v.Ports,
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
