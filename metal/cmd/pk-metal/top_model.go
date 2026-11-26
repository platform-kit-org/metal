package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"k8s.io/apimachinery/pkg/types"
)

// The terminal half of pk-metal top: a Bubble Tea model. Update and View are
// plain functions of the model, so tests drive them with fixed messages and
// compare the view at a fixed width; nothing here needs a terminal.

// topSource reads everything the dashboard shows, from caches that watches
// keep current and from the fleet's state endpoint.
type topSource interface {
	Read(ctx context.Context) topInput
}

// topActions are the keys that change something. They call the same code as
// pk-metal approve, hold, unhold and retry.
type topActions interface {
	// Plan returns the plan of an operation as pk-metal approve prints it,
	// who would approve it, and which object it was read from; an error when
	// the caller cannot approve it.
	Plan(ctx context.Context, namespace, name string) (shownPlan, error)
	// Approve approves the operation as the plan's user, if it is still the
	// same object and its plan still reads the same.
	Approve(ctx context.Context, namespace, name string, p shownPlan) (string, error)
	Hold(ctx context.Context, namespace, name string, on bool) (string, error)
	Retry(ctx context.Context, namespace, name string) (string, error)
}

// Messages.
type (
	topDataMsg   topInput
	topTickMsg   struct{}
	topChangeMsg struct{}
	topPlanMsg   struct {
		namespace, name string
		plan            shownPlan
		err             error
	}
	topDoneMsg struct {
		text string
		err  error
	}
)

type topMode int

const (
	modeTiles topMode = iota
	modePlan          // an approval plan is shown and a y/N answer awaited
)

type topModel struct {
	src     topSource
	act     topActions
	changes <-chan struct{} // a watched object changed
	every   time.Duration   // how often the fleet and consoles are read again

	data    topData
	loaded  bool
	reading bool
	width   int
	height  int
	sel     int
	mode    topMode
	busy    bool // an action runs

	plan             shownPlan
	planNS, planName string
	planAt           time.Time // when the plan was put on the screen
	status           string
	statusBad        bool
	// now is the clock the approval window is measured with; nil is
	// time.Now.
	now func() time.Time
	// actTimeout bounds one key's call to the cluster; zero is
	// topActionTimeout. Tests shorten it.
	actTimeout time.Duration
}

// topActionTimeout is how long a key's call (plan, approve, hold, retry) may
// take. Without a bound a hung API server would leave the screen busy for
// good, and every other key with it.
const topActionTimeout = 30 * time.Second

// bounded runs a key's call with a deadline. A call that runs out says so, and
// that its outcome is not known: an approval may have reached the cluster
// just before the answer was lost.
func (m topModel) bounded(call func(ctx context.Context) tea.Msg) tea.Cmd {
	limit := m.actTimeout
	if limit == 0 {
		limit = topActionTimeout
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		msg := call(ctx)
		if ctx.Err() == nil {
			return msg
		}
		err := fmt.Errorf("the cluster did not answer within %s; the outcome is not known, look again before you act", shortDuration(limit))
		if p, ok := msg.(topPlanMsg); ok {
			p.err = err
			return p
		}
		return topDoneMsg{err: err}
	}
}

// shownPlan is an approval plan as a person sees it: the text, who would
// approve, and the operation object it was read from.
type shownPlan struct {
	Text, User string
	UID        types.UID
}

func (m topModel) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func newTopModel(src topSource, act topActions, changes <-chan struct{}, every time.Duration) topModel {
	return topModel{src: src, act: act, changes: changes, every: every}
}

func (m topModel) Init() tea.Cmd {
	return tea.Batch(m.read(), m.waitChange())
}

// read fetches a new topInput, unless a read is under way.
func (m *topModel) read() tea.Cmd {
	if m.src == nil || m.reading {
		return nil
	}
	m.reading = true
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return topDataMsg(src.Read(ctx))
	}
}

func (m topModel) waitChange() tea.Cmd {
	if m.changes == nil {
		return nil
	}
	ch := m.changes
	return func() tea.Msg {
		<-ch
		return topChangeMsg{}
	}
}

func (m topModel) tick() tea.Cmd {
	if m.every <= 0 {
		return nil
	}
	return tea.Tick(m.every, func(time.Time) tea.Msg { return topTickMsg{} })
}

func (m topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case topDataMsg:
		m.reading, m.loaded = false, true
		m.data = buildTop(topInput(msg))
		if m.sel >= len(m.data.Tiles) {
			m.sel = max(0, len(m.data.Tiles)-1)
		}
		return m, m.tick()
	case topTickMsg:
		return m, m.read()
	case topChangeMsg:
		return m, tea.Batch(m.read(), m.waitChange())
	case topPlanMsg:
		m.busy = false
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
			return m, nil
		}
		m.mode, m.plan, m.planNS, m.planName = modePlan, msg.plan, msg.namespace, msg.name
		m.planAt = m.clock()
		m.status = ""
		return m, nil
	case topDoneMsg:
		m.busy = false
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
		} else {
			m.setStatus(msg.text, false)
		}
		return m, m.read()
	case tea.KeyPressMsg:
		return m.key(msg.String())
	}
	return m, nil
}

func (m *topModel) setStatus(s string, bad bool) {
	m.status, m.statusBad = strings.TrimSpace(s), bad
}

func (m topModel) selected() *tile {
	if m.sel < 0 || m.sel >= len(m.data.Tiles) {
		return nil
	}
	return &m.data.Tiles[m.sel]
}

func (m topModel) key(k string) (tea.Model, tea.Cmd) {
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	if m.mode == modePlan {
		// Only an explicit yes approves; every other key leaves the
		// operation as it was, as pk-metal approve does.
		ns, name, plan := m.planNS, m.planName, m.plan
		m.mode, m.plan = modeTiles, shownPlan{}
		if k != "y" && k != "Y" {
			m.setStatus(errDeclined.Error(), false)
			return m, nil
		}
		// The same window as pk-metal approve's question: a plan left on
		// the screen longer is read again before anything is approved.
		if m.clock().Sub(m.planAt) > approvalWindow {
			m.setStatus(fmt.Sprintf("the plan of %s/%s was shown more than %s ago; nothing changed. Press a to see it as it is now",
				ns, name, shortDuration(approvalWindow)), true)
			return m, nil
		}
		m.busy = true
		m.setStatus("approving "+ns+"/"+name+" …", false)
		act := m.act
		return m, m.bounded(func(ctx context.Context) tea.Msg {
			text, err := act.Approve(ctx, ns, name, plan)
			return topDoneMsg{text, err}
		})
	}
	n := len(m.data.Tiles)
	switch k {
	case "q", "esc":
		return m, tea.Quit
	case "tab", "right", "down", "l", "j":
		if n > 0 {
			m.sel = (m.sel + 1) % n
		}
		return m, nil
	case "shift+tab", "left", "up", "k":
		if n > 0 {
			m.sel = (m.sel + n - 1) % n
		}
		return m, nil
	}
	if m.busy || m.act == nil {
		return m, nil
	}
	t := m.selected()
	if t == nil {
		return m, nil
	}
	act := m.act
	switch k {
	case "a":
		if t.Op == nil || !t.Op.Waits {
			m.setStatus(t.Title+": no operation waits for approval", false)
			return m, nil
		}
		ns, name := t.Op.Namespace, t.Op.Name
		m.busy = true
		m.setStatus("reading the plan of "+ns+"/"+name+" …", false)
		return m, m.bounded(func(ctx context.Context) tea.Msg {
			plan, err := act.Plan(ctx, ns, name)
			return topPlanMsg{namespace: ns, name: name, plan: plan, err: err}
		})
	case "h":
		if t.ClaimName == "" {
			m.setStatus(t.Title+": no claim to hold", false)
			return m, nil
		}
		ns, name, on := t.ClaimNS, t.ClaimName, !t.OnHold
		m.busy = true
		return m, m.bounded(func(ctx context.Context) tea.Msg {
			text, err := act.Hold(ctx, ns, name, on)
			return topDoneMsg{text, err}
		})
	case "r":
		if t.Op == nil || !t.Op.Failed {
			m.setStatus(t.Title+": no failed operation to retry", false)
			return m, nil
		}
		ns, name := t.Op.Namespace, t.Op.Name
		m.busy = true
		return m, m.bounded(func(ctx context.Context) tea.Msg {
			text, err := act.Retry(ctx, ns, name)
			return topDoneMsg{text, err}
		})
	}
	return m, nil
}

// Styles. The marks carry the meaning; colour only repeats it.
var (
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Green)
	styleWait  = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	styleBad   = lipgloss.NewStyle().Foreground(lipgloss.Red).Bold(true)
	styleIdle  = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	styleTitle = lipgloss.NewStyle().Bold(true)
	styleDim   = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	styleTile  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.BrightBlack).Padding(0, 1)
	styleSel   = lipgloss.NewStyle().Border(lipgloss.ThickBorder()).BorderForeground(lipgloss.Cyan).Padding(0, 1)
	styleBox   = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Padding(0, 1)
	// styleDetail is the panel of the selected machine: the colour of the
	// selected tile ties the two together.
	styleDetail = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Cyan).Padding(0, 1)
)

func (s state) style() lipgloss.Style {
	switch s {
	case stOK:
		return styleOK
	case stWait:
		return styleWait
	case stBad:
		return styleBad
	}
	return styleIdle
}

// marked is a fact with its mark, coloured by its state.
func marked(s state, text string) string {
	return s.style().Render(s.mark() + " " + text)
}

// defaultTopWidth is the width the view assumes before the terminal says.
const defaultTopWidth = 100

func (m topModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

// render is the whole screen as text.
func (m topModel) render() string {
	width := m.width
	if width <= 0 {
		width = defaultTopWidth
	}
	var b strings.Builder
	b.WriteString(m.header(width))
	b.WriteString("\n")
	switch {
	case m.mode == modePlan:
		box := styleBox.Width(width)
		body := m.plan.Text + "\n" + styleTitle.Render(fmt.Sprintf("Approve as %s? [y/N]", m.plan.User))
		b.WriteString(box.Render(body))
	case !m.loaded:
		b.WriteString("reading the cluster …")
	case len(m.data.Tiles) == 0:
		b.WriteString("no machines: no Host in the cluster and no fleet machine")
	default:
		// The tiles, then the selected machine in full: it takes the lines
		// the header, the tiles and the footer leave.
		grid := m.grid(width)
		b.WriteString(grid)
		if t := m.selected(); t != nil {
			left := 0
			if m.height > 0 {
				left = m.height - lipgloss.Height(b.String()) - lipgloss.Height(m.footer(width))
			}
			b.WriteString("\n")
			b.WriteString(m.detail(*t, width, left))
		}
	}
	b.WriteString("\n")
	b.WriteString(m.footer(width))
	return b.String()
}

func (m topModel) header(width int) string {
	first := styleTitle.Render("pk-metal top") + "   " + m.data.NodeLine
	if m.data.Attention > 0 {
		first += "   " + styleBad.Render(fmt.Sprintf("needs a person: %d", m.data.Attention))
	} else {
		first += "   " + styleOK.Render("needs a person: 0")
	}
	lines := []string{first}
	if len(m.data.Clusters) == 0 {
		lines = append(lines, styleDim.Render("no TalosCluster"))
	}
	for _, c := range m.data.Clusters {
		avail := marked(c.Available, "Available")
		if c.Why != "" {
			avail += " " + styleWait.Render("("+c.Why+")")
		}
		lines = append(lines, fmt.Sprintf("cluster %s: %s  %s  control planes %d/%d, workers %d/%d",
			c.Name, marked(c.Bootstrapped, "Bootstrapped"), avail,
			c.CPReady, c.ControlPlanes, c.WorkersReady, c.Workers))
	}
	for _, r := range m.data.Rollouts {
		st := stWait
		if r.NeedsAttention {
			st = stBad
		}
		lines = append(lines, fmt.Sprintf("rollout %s (%s, cluster %s): %s  nodes %d/%d",
			r.Name, r.Kind, r.Cluster, marked(st, r.State), r.NodesDone, r.NodesTotal))
	}
	if m.data.FleetLine != "" {
		lines = append(lines, styleDim.Render(m.data.FleetLine))
	}
	if len(m.data.Loading) > 0 {
		lines = append(lines, styleWait.Render("still reading: "+strings.Join(m.data.Loading, ", ")))
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	return strings.Join(lines, "\n")
}

// tileWidth is the least outer width of one tile; as many as fit go side by
// side, and they share what is left over.
const tileWidth = 40

func (m topModel) grid(width int) string {
	cols := max(1, min(width/tileWidth, len(m.data.Tiles)))
	w := max(tileWidth, width/cols)
	if cols == 1 {
		w = width
	}
	var rows []string
	for start := 0; start < len(m.data.Tiles); start += cols {
		var row []string
		for i := start; i < min(start+cols, len(m.data.Tiles)); i++ {
			row = append(row, m.renderTile(m.data.Tiles[i], i == m.sel, w))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, row...))
	}
	return strings.Join(rows, "\n")
}

// consoleLines is the least number of console lines the detail panel shows;
// a taller terminal shows more.
const consoleLines = 3

// renderTile is one machine at a glance, in four lines: what it is, where
// along the flow it is, the Host, and what happens to it now. The detail
// panel below the tiles has the rest.
func (m topModel) renderTile(t tile, selected bool, w int) string {
	style := styleTile
	if selected {
		style = styleSel
	}
	inner := w - style.GetHorizontalFrameSize()
	l := []string{titleLine(t), flowLine(t, false), hostLine(t)}

	switch {
	case t.Op != nil:
		text := "op " + t.Op.Step
		if t.Op.Waits {
			text += ", waits for approval"
		}
		l = append(l, marked(t.Op.state(), text))
	case t.NodeName != "":
		node := t.Flow[len(t.Flow)-1]
		l = append(l, marked(node.State, t.NodeName+" "+node.Text))
	default:
		l = append(l, styleDim.Render("no operation, no node"))
	}
	for i := range l {
		l[i] = ansi.Truncate(l[i], inner, "…")
	}
	return style.Width(w).Render(strings.Join(l, "\n"))
}

func titleLine(t tile) string {
	title := styleTitle.Render(t.Title)
	if t.Subtitle != "" {
		title += " " + styleDim.Render(t.Subtitle)
	}
	return title
}

// flowLine is the flow BMC → media → VM → node as marks; with words, what
// each mark stands for too.
func flowLine(t tile, words bool) string {
	var parts []string
	for _, st := range t.Flow {
		part := st.Name + " " + st.State.style().Render(st.State.mark())
		if words {
			part = st.Name + " " + marked(st.State, st.Text)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, styleDim.Render(" ─▶ "))
}

// hostLine is the Host's phase, what blocks it, and its hold.
func hostLine(t tile) string {
	host := orDash(t.Phase)
	if t.BlockedOn != "" {
		host += " · " + t.BlockedOn
	}
	line := marked(t.hostState(), host)
	if t.OnHold {
		line += "  " + styleWait.Render("ON HOLD")
	}
	return line
}

// state is how an operation reads.
func (o *opLine) state() state {
	switch {
	case o.Failed:
		return stBad
	case o.Waits:
		return stWait
	}
	return stOK
}

// detailLabel is the width of the labels in the detail panel.
const detailLabel = 8

// detailColumns is the least inner width at which the detail panel shows
// the machine and the cluster side by side.
const detailColumns = 100

// detail is the selected machine in full, as wide as the terminal: every
// field, long messages wrapped, and as many console lines as the height
// leaves room for. height is the number of lines the panel may take; zero
// or less shows the least.
func (m topModel) detail(t tile, width, height int) string {
	style := styleDetail
	inner := width - style.GetHorizontalFrameSize()
	machine, cluster := t.machineFields(), t.clusterFields()

	l := []string{titleLine(t), flowLine(t, true)}
	if t.Sim && inner >= detailColumns {
		// Side by side: the machine on the left, the cluster on the right.
		half := (inner - 2) / 2
		left := fieldLines(machine, half)
		right := fieldLines(cluster, inner-half-2)
		for i := 0; i < max(len(left), len(right)); i++ {
			var a, b string
			if i < len(left) {
				a = left[i]
			}
			if i < len(right) {
				b = right[i]
			}
			l = append(l, a+strings.Repeat(" ", max(0, half-ansi.StringWidth(a)))+"  "+b)
		}
	} else {
		l = append(l, fieldLines(append(machine, cluster...), inner)...)
	}

	if t.Sim {
		l = append(l, styleDim.Render(fmt.Sprintf("%-*s", detailLabel, "console"))+orDash(t.ConsoleLog))
		n := max(consoleLines, height-style.GetVerticalFrameSize()-len(l))
		lines := t.Console
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		for i := 0; i < n; i++ {
			s := ""
			if i < len(lines) {
				s = lines[i]
			}
			l = append(l, styleDim.Render("│ ")+s)
		}
	}
	for i := range l {
		l[i] = ansi.Truncate(l[i], inner, "…")
	}
	return style.Width(width).Render(strings.Join(l, "\n"))
}

// field is one labelled fact of the detail panel. A fact that does not fit
// its column goes on over the next lines when wrap is set, and is cut short
// otherwise.
type field struct {
	label, text string
	wrap        bool
}

// fieldLines lays fields out in a column of the given width.
func fieldLines(fs []field, width int) []string {
	var out []string
	for _, f := range fs {
		label := styleDim.Render(fmt.Sprintf("%-*s", detailLabel, f.label))
		room := max(1, width-detailLabel)
		parts := []string{f.text}
		if f.wrap {
			parts = strings.Split(ansi.Wrap(f.text, room, " /,"), "\n")
		}
		for i, p := range parts {
			if i > 0 {
				label = strings.Repeat(" ", detailLabel)
			}
			out = append(out, ansi.Truncate(label+p, width, "…"))
		}
	}
	return out
}

// machineFields are the BMC and, with the simulator, the machine behind it.
func (t tile) machineFields() []field {
	bmc := orDash(hostPort(t.BMCAddress))
	if t.Sim {
		bmc += fmt.Sprintf("  power %s  boot %s", strings.ToLower(t.Power), t.Override)
	} else if t.BMCPhase != "" {
		bmc += "  " + t.BMCPhase
	}
	fs := []field{{label: "BMC", text: bmc}}
	if !t.Sim {
		return fs
	}
	if len(t.Media) == 0 {
		fs = append(fs, field{label: "media", text: "none"})
	}
	for i, md := range t.Media {
		fs = append(fs, field{label: map[bool]string{true: "media"}[i == 0], text: md})
	}
	fs = append(fs, field{label: "last", text: orDash(t.LastCall)})
	vm := t.VMState.style().Render(t.VMState.mark()) + " " + t.VM
	if t.HangCycles > 0 {
		vm += " · " + styleBad.Render(fmt.Sprintf("hang-cycled %d×", t.HangCycles))
	}
	return append(fs, field{label: "VM", text: vm})
}

// clusterFields are the Host, its claim, its operation and its node.
func (t tile) clusterFields() []field {
	fs := []field{{label: "host", text: hostLine(t)}}
	if t.hostState() == stBad && t.Message != "" {
		fs = append(fs, field{text: styleBad.Render(t.Message), wrap: true})
	}
	claim := "-"
	if t.ClaimName != "" {
		claim = t.ClaimNS + "/" + t.ClaimName
	}
	fs = append(fs, field{label: "claim", text: claim})

	if t.Op == nil {
		fs = append(fs, field{label: "op", text: "-"})
	} else {
		fs = append(fs,
			field{label: "op", text: t.Op.state().style().Render(t.Op.Step + ", " + t.Op.Approval)},
			field{text: styleDim.Render(t.Op.Namespace + "/" + t.Op.Name)})
		if t.Op.Failed && t.Op.Message != "" {
			fs = append(fs, field{text: styleBad.Render(t.Op.Message), wrap: true})
		}
	}

	if t.NodeName == "" {
		return append(fs, field{label: "node", text: "-"})
	}
	talos := "Talos ready"
	if t.TalosReady != stOK {
		talos = "Talos " + orDash(t.TalosWhy)
	}
	ready := marked(t.TalosReady, talos)
	if t.K8sKnown {
		ready += "  " + marked(t.K8sReady, map[bool]string{true: "kubelet Ready", false: "kubelet not Ready"}[t.K8sReady == stOK])
	}
	return append(fs,
		field{label: "node", text: strings.TrimSpace(t.NodeName + " " + t.NodeAddress)},
		field{text: ready})
}

func (m topModel) footer(width int) string {
	keys := "tab select  a approve  h hold/unhold  r retry  q quit"
	if m.mode == modePlan {
		keys = "y approve  any other key: do not approve"
	}
	if m.act == nil {
		keys = "tab select  q quit"
	}
	lines := []string{styleDim.Render(keys)}
	if m.status != "" {
		st := styleOK
		if m.statusBad {
			st = styleBad
		}
		lines = append(lines, st.Render(oneLine(m.status)))
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	return strings.Join(lines, "\n")
}

// oneLine joins the lines of an action's output: the status line has room
// for one.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
