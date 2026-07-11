package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"agent-ssh/internal/client"
	"agent-ssh/internal/config"
	"agent-ssh/internal/model"
	"agent-ssh/internal/protocol"
)

var (
	ensureDaemonFn = client.EnsureDaemon
	connectFn      = client.Connect
	requestFn      = client.Request
)

type page int

const (
	pageReview page = iota + 1
	pageActivity
	pageConnections
	pagePolicy
	pageSettings
)

type app struct {
	safe        bool
	page        page
	selected    int
	width       int
	height      int
	commands    map[string]model.CommandRecord
	connections []model.ConnectionStatus
	rules       []model.PolicyRuleView
	paused      bool
	mode        string
	message     string
	help        bool
	filter      string
	inputMode   inputMode
	confirm     *confirmation
	confirmRisk map[model.Risk]struct{}
	viewport    viewport.Model
	sub         *subscription
	styles      styles
}

type inputMode int

const (
	inputNone inputMode = iota
	inputFilter
)

type confirmation struct {
	title   string
	body    string
	action  string
	payload protocol.Message
	sending bool
}

type subscription struct {
	conn io.Closer
	ch   <-chan protocol.Message
	err  <-chan error
}

type daemonMsg protocol.Message
type daemonErr error

func Run(args []string) int {
	safe := false
	for _, arg := range args {
		switch arg {
		case "--safe":
			safe = true
		default:
			fmt.Fprintf(os.Stderr, "agent-ssh: unknown tui arg %q\n", arg)
			return 2
		}
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-ssh: %v\n", err)
		return 1
	}
	if err := ensureDaemonFn(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "agent-ssh: %v\n", err)
		return 1
	}
	sub, err := newSubscription()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-ssh: %v\n", err)
		return 1
	}
	defer sub.conn.Close()

	m := newApp(safe, sub, cfg.Approval.ConfirmRisks)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-ssh tui: %v\n", err)
		return 1
	}
	return 0
}

func newApp(safe bool, sub *subscription, confirmRisks []string) app {
	vp := viewport.New(80, 20)
	confirmRisk := make(map[model.Risk]struct{}, len(confirmRisks))
	for _, risk := range confirmRisks {
		confirmRisk[model.Risk(risk)] = struct{}{}
	}
	return app{
		safe:        safe,
		page:        pageReview,
		commands:    map[string]model.CommandRecord{},
		mode:        config.ModeSensitive,
		confirmRisk: confirmRisk,
		viewport:    vp,
		sub:         sub,
		styles:      newStyles(),
	}
}

func newSubscription() (*subscription, error) {
	conn, err := connectFn()
	if err != nil {
		return nil, fmt.Errorf("connect daemon: %w", err)
	}
	enc := protocol.NewEncoder(conn)
	dec := protocol.NewDecoder(conn)
	if err := enc.Encode(protocol.Message{Type: "subscribe_events"}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("subscribe events: %w", err)
	}
	msgCh := make(chan protocol.Message, 100)
	errCh := make(chan error, 1)
	go func() {
		for {
			var msg protocol.Message
			if err := dec.Decode(&msg); err != nil {
				errCh <- err
				return
			}
			msgCh <- msg
		}
	}()
	return &subscription{conn: conn, ch: msgCh, err: errCh}, nil
}

func (a app) Init() tea.Cmd {
	return tea.Batch(a.waitDaemonMsg(), a.waitDaemonErr())
}

func (a app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.viewport.Width = msg.Width
		a.viewport.Height = max(1, msg.Height-4)
		return a, nil
	case tea.KeyMsg:
		cmd := a.handleKey(msg)
		return a, cmd
	case daemonMsg:
		a.apply(protocol.Message(msg))
		a.clampSelection()
		return a, a.waitDaemonMsg()
	case daemonErr:
		if msg != nil && error(msg) != io.EOF {
			a.message = "daemon event stream closed: " + error(msg).Error()
		}
		return a, a.waitDaemonErr()
	}
	return a, nil
}

func (a app) View() string {
	if a.width == 0 {
		return "loading agent-ssh..."
	}
	content := a.content()
	a.viewport.SetContent(content)
	header := a.header()
	footer := a.footer()
	return lipgloss.JoinVertical(lipgloss.Left, header, a.viewport.View(), footer)
}

func (a app) waitDaemonMsg() tea.Cmd {
	return func() tea.Msg {
		msg := <-a.sub.ch
		return daemonMsg(msg)
	}
}

func (a app) waitDaemonErr() tea.Cmd {
	return func() tea.Msg {
		err := <-a.sub.err
		return daemonErr(err)
	}
}

func (a *app) handleKey(msg tea.KeyMsg) tea.Cmd {
	if a.confirm != nil {
		return a.handleConfirmKey(msg)
	}
	if a.inputMode == inputFilter {
		return a.handleFilterKey(msg)
	}
	switch msg.String() {
	case "ctrl+c", "q":
		return tea.Quit
	case "1":
		a.setPage(pageReview)
	case "2":
		a.setPage(pageActivity)
	case "3":
		a.setPage(pageConnections)
	case "4":
		a.setPage(pagePolicy)
	case "5":
		a.setPage(pageSettings)
	case "j", "down":
		a.selected++
		a.clampSelection()
	case "k", "up":
		if a.selected > 0 {
			a.selected--
		}
	case "?":
		a.help = !a.help
	case "/":
		a.inputMode = inputFilter
		a.message = "filter: " + a.filter
	case "a":
		return a.decide("approve")
	case "r":
		return a.decide("reject")
	case "x":
		a.confirmCancel()
	case "p":
		return a.togglePause()
	case "!":
		a.confirmEmergencyStop()
	case "m":
		a.confirmModeChange()
	case "d":
		return a.closeSelectedConnection()
	case "D":
		return a.closeIdleConnections()
	}
	return nil
}

func (a *app) handleConfirmKey(msg tea.KeyMsg) tea.Cmd {
	if a.confirm.sending {
		if msg.String() == "ctrl+c" {
			return tea.Quit
		}
		return nil
	}
	switch msg.String() {
	case "esc", "n", "N":
		a.message = "cancelled " + a.confirm.action
		a.confirm = nil
	case "enter":
		payload := a.confirm.payload
		a.message = a.confirm.action + " requested"
		a.confirm.sending = true
		return requestCmd(payload)
	}
	return nil
}

func (a *app) handleFilterKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter", "esc":
		a.inputMode = inputNone
		if a.filter == "" {
			a.message = "filter cleared"
		} else {
			a.message = "filter: " + a.filter
		}
	case "backspace", "ctrl+h":
		if len(a.filter) > 0 {
			a.filter = a.filter[:len(a.filter)-1]
		}
	case "ctrl+u":
		a.filter = ""
	default:
		for _, r := range msg.String() {
			if unicode.IsPrint(r) {
				a.filter += string(r)
			}
		}
	}
	a.selected = 0
	a.message = "filter: " + a.filter
	return nil
}

func (a *app) setPage(p page) {
	a.page = p
	a.selected = 0
	a.message = ""
	a.viewport.GotoTop()
}

func (a *app) apply(msg protocol.Message) {
	if a.confirm != nil && a.confirm.sending {
		switch msg.Type {
		case "ui.error":
			a.confirm.sending = false
			a.message = msg.Error
			return
		case "ack":
			a.confirm = nil
			return
		}
	}
	if msg.Type == "ui.error" {
		a.message = msg.Error
		return
	}
	switch msg.Type {
	case "snapshot":
		a.paused = msg.Paused
		if msg.Mode != "" {
			a.mode = msg.Mode
		}
		a.commands = map[string]model.CommandRecord{}
		for _, rec := range msg.Commands {
			a.commands[rec.ID] = rec
		}
		a.connections = msg.Connections
		a.rules = msg.PolicyRules
	case "daemon.paused":
		a.paused = msg.Paused
	case "policy.mode_changed":
		if msg.Mode != "" {
			a.mode = msg.Mode
		}
	case "connection.updated":
		if snap, err := requestFn(context.Background(), protocol.Message{Type: "snapshot"}); err == nil {
			a.apply(snap)
		}
	default:
		if msg.Record != nil {
			a.commands[msg.Record.ID] = *msg.Record
		}
	}
}

func (a *app) decide(decision string) tea.Cmd {
	rec, ok := a.selectedCommand()
	if !ok {
		a.message = "no command selected"
		return nil
	}
	if rec.Status != model.StatusPendingApproval {
		a.message = "selected command is not pending approval"
		return nil
	}
	if decision == "approve" {
		if _, ok := a.confirmRisk[rec.Risk]; ok {
			a.confirmApproval(rec)
			return nil
		}
	}
	a.message = decision + " requested for " + shortID(rec.ID)
	return requestCmd(protocol.Message{Type: "approval.decide", ID: rec.ID, Decision: decision})
}

func (a *app) confirmApproval(rec model.CommandRecord) {
	body := fmt.Sprintf("Host: %s\nRisk: %s\nRule: %s\n\nCommand:\n%s\n\n[Enter] Approve    [Esc] Go back", rec.Host, strings.ToUpper(string(rec.Risk)), emptyDash(rec.PolicyRule), rec.Command)
	a.confirm = &confirmation{title: "APPROVE COMMAND?", body: body, action: "approve", payload: protocol.Message{Type: "approval.decide", ID: rec.ID, Decision: "approve"}}
	a.message = "confirm approve"
}

func (a *app) cancelSelected() tea.Cmd {
	rec, ok := a.selectedCommand()
	if !ok {
		a.message = "no command selected"
		return nil
	}
	a.message = "cancel requested for " + shortID(rec.ID)
	return requestCmd(protocol.Message{Type: "command.cancel", ID: rec.ID})
}

func (a *app) confirmCancel() {
	rec, ok := a.selectedCommand()
	if !ok {
		a.message = "no command selected"
		return
	}
	body := fmt.Sprintf("Cancel command %s on %s?\n\nStatus: %s\nCommand: %s\n\n[Enter] Cancel command    [Esc] Go back", rec.ID, rec.Host, rec.Status, rec.Command)
	a.confirm = &confirmation{title: "Cancel command?", body: body, action: "cancel", payload: protocol.Message{Type: "command.cancel", ID: rec.ID}}
	a.message = "confirm cancel"
}

func (a *app) togglePause() tea.Cmd {
	a.message = "toggle pause requested"
	return requestCmd(protocol.Message{Type: "daemon.pause", Paused: !a.paused})
}

func (a *app) emergencyStop() tea.Cmd {
	a.message = "emergency stop requested"
	return requestCmd(protocol.Message{Type: "daemon.emergency_stop"})
}

func (a *app) confirmEmergencyStop() {
	a.confirm = &confirmation{
		title:   "Emergency stop?",
		body:    "This pauses new commands and cancels pending/queued commands. Running commands are listed for manual cancellation.\n\n[Enter] Emergency stop    [Esc] Go back",
		action:  "emergency stop",
		payload: protocol.Message{Type: "daemon.emergency_stop"},
	}
	a.message = "confirm emergency stop"
}

func (a *app) cycleMode() tea.Cmd {
	next := nextMode(a.mode)
	a.message = "mode change requested: " + next
	return requestCmd(protocol.Message{Type: "policy.set_mode", Mode: next})
}

func (a *app) confirmModeChange() {
	next := nextMode(a.mode)
	body := fmt.Sprintf("Switch execution mode from %s to %s?\n\nAuto runs sensitive commands without approval. Block rules still apply.\n\n[Enter] Switch mode    [Esc] Go back", a.mode, next)
	a.confirm = &confirmation{title: "Switch mode?", body: body, action: "mode change", payload: protocol.Message{Type: "policy.set_mode", Mode: next}}
	a.message = "confirm mode change"
}

func (a *app) closeSelectedConnection() tea.Cmd {
	if a.page != pageConnections || len(a.connections) == 0 {
		a.message = "select a connection first"
		return nil
	}
	a.clampSelection()
	conn := a.connections[a.selected]
	if conn.OpenSessions > 0 {
		a.message = "connection has active sessions; cancel running commands first"
		return nil
	}
	a.message = "disconnect requested for " + conn.Host
	return requestCmd(protocol.Message{Type: "connections.close_host", Host: conn.Host})
}

func (a *app) closeIdleConnections() tea.Cmd {
	a.message = "close all idle connections requested"
	return requestCmd(protocol.Message{Type: "connections.close_idle"})
}

func requestCmd(req protocol.Message) tea.Cmd {
	return func() tea.Msg {
		resp, err := requestFn(context.Background(), req)
		if err != nil {
			return daemonMsg(protocol.Message{Type: "ui.error", Error: err.Error()})
		}
		if !resp.OK && resp.Error != "" {
			return daemonMsg(protocol.Message{Type: "ui.error", Error: resp.Error})
		}
		return daemonMsg(resp)
	}
}

func (a *app) header() string {
	pending, running, failed := 0, 0, 0
	for _, rec := range a.commands {
		switch rec.Status {
		case model.StatusPendingApproval:
			pending++
		case model.StatusRunning:
			running++
		case model.StatusFailed, model.StatusTimeout, model.StatusBlocked, model.StatusCancelFailed:
			failed++
		}
	}
	status := ""
	if a.paused {
		status = a.styles.danger.Render(" PAUSED ")
	} else {
		status = a.styles.ok.Render(" ACTIVE ")
	}
	tabs := []string{"1 Review", "2 Activity", "3 Connections", "4 Policy", "5 Settings"}
	for i := range tabs {
		if page(i+1) == a.page {
			tabs[i] = a.styles.tabActive.Render(tabs[i])
		} else {
			tabs[i] = a.styles.tab.Render(tabs[i])
		}
	}
	line := fmt.Sprintf("agent-ssh %s mode=%s pending=%d running=%d attention=%d connections=%d", status, a.styles.mode(a.mode).Render(strings.ToUpper(a.mode)), pending, running, failed, len(a.connections))
	return a.styles.header.Width(a.width).Render(line) + "\n" + strings.Join(tabs, " ")
}

func (a *app) footer() string {
	msg := a.message
	if msg == "" {
		msg = "a approve  r reject  x cancel  p pause  ! stop  m mode  d disconnect  D close-idle  ? help  q quit"
	}
	return a.styles.footer.Width(a.width).Render(msg)
}

func (a app) content() string {
	var content string
	if a.help {
		content = a.helpView()
	} else {
		switch a.page {
		case pageReview:
			content = a.reviewView()
		case pageActivity:
			content = a.activityView()
		case pageConnections:
			content = a.connectionsView()
		case pagePolicy:
			content = a.policyView()
		case pageSettings:
			content = a.settingsView()
		default:
			content = ""
		}
	}
	if a.confirm != nil {
		content += "\n" + a.styles.dialog.Render(a.styles.title.Render(a.confirm.title)+"\n\n"+a.confirm.body)
	}
	return content
}

func (a app) reviewView() string {
	commands := a.visibleCommands()
	if len(commands) == 0 {
		return a.styles.panel.Render("Pending Review\n\nNo commands waiting for approval.\n\nSensitive commands will appear here when an agent runs them.")
	}
	if a.safe || a.width < 120 {
		return a.commandTable("Pending Review", commands, true, a.width) + "\n" + commandDetail(a.selectedOrZero(commands), a.styles)
	}
	tableWidth := a.width/2 - 2
	left := lipgloss.NewStyle().Width(tableWidth).Render(a.commandTable("Pending Review", commands, true, tableWidth))
	right := lipgloss.NewStyle().Width(a.width/2 - 2).Render(commandDetail(a.selectedOrZero(commands), a.styles))
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func (a app) activityView() string {
	return a.commandTable("Activity", a.visibleCommands(), false, a.width)
}

func (a app) connectionsView() string {
	var b strings.Builder
	b.WriteString(a.styles.title.Render("Connections"))
	b.WriteString("\n\n")
	b.WriteString(a.styles.muted.Render("d disconnect selected idle connection, D disconnect all idle connections"))
	b.WriteString("\n\n")
	b.WriteString(row("", "HOST", "STATUS", "USER", "ADDR", "SESS", "IDLE", "LAST COMMAND"))
	if len(a.connections) == 0 {
		b.WriteString("\n")
		b.WriteString(a.styles.panel.Render("No active SSH connections."))
		return b.String()
	}
	for i, conn := range a.connections {
		marker := " "
		if i == a.selected {
			marker = a.styles.selected.Render(">")
		}
		idle := "-"
		if !conn.LastUsed.IsZero() {
			idle = time.Since(conn.LastUsed).Round(time.Second).String()
		}
		b.WriteString(row(marker, conn.Host, a.styles.status(conn.Status).Render(conn.Status), conn.User, conn.Addr, fmt.Sprint(conn.OpenSessions), idle, truncate(conn.LastCommand, 54)))
	}
	return b.String()
}

func (a app) policyView() string {
	var b strings.Builder
	b.WriteString(a.styles.title.Render("Policy"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("Mode: %s\nDefault in sensitive mode: allow unmatched commands\nBuiltin rules: enabled\n\n", a.styles.mode(a.mode).Render(strings.ToUpper(a.mode))))
	b.WriteString(row("", "RULE", "ACTION", "RISK", "MATCHES", "PATTERNS", "", ""))
	for _, rule := range a.rules {
		patterns := strings.Join(rule.Patterns, "; ")
		b.WriteString(row("", rule.Name, string(rule.Action), a.styles.risk(rule.Risk).Render(strings.ToUpper(string(rule.Risk))), fmt.Sprint(rule.Matches), truncate(patterns, 90), "", ""))
	}
	if len(a.rules) == 0 {
		b.WriteString("\n")
		b.WriteString(a.styles.panel.Render("No policy rules loaded."))
	}
	return b.String()
}

func (a app) settingsView() string {
	paused := a.styles.ok.Render("off")
	if a.paused {
		paused = a.styles.danger.Render("on")
	}
	items := []string{
		"Execution Mode: " + a.styles.mode(a.mode).Render(strings.ToUpper(a.mode)),
		"Paused: " + paused,
		fmt.Sprintf("Safe layout: %t", a.safe),
		"Mode cycle: sensitive -> approval -> auto",
		"Mode changes are persisted to ~/.config/agent-ssh/config.yaml",
		"Audit output retention: summary with redaction",
	}
	return a.styles.title.Render("Settings") + "\n\n" + a.styles.panel.Render(strings.Join(items, "\n"))
}

func (a app) helpView() string {
	return a.styles.panel.Render(strings.Join([]string{
		"agent-ssh keys",
		"",
		"1-5        switch page",
		"j/k        move selection",
		"a          approve selected; configured risks require Enter",
		"r          reject selected pending command immediately",
		"x          cancel selected pending/queued/running command",
		"p          pause/resume new commands",
		"!          emergency stop: pause and cancel pending/queued commands",
		"m          cycle execution mode",
		"/          filter commands",
		"d          disconnect selected idle connection",
		"D          disconnect all idle connections",
		"?          toggle help",
		"q          quit TUI only; daemon keeps running",
	}, "\n"))
}

func (a app) commandTable(title string, commands []model.CommandRecord, review bool, width int) string {
	widths := commandTableWidths(width)
	var b strings.Builder
	b.WriteString(a.styles.title.Render(title))
	b.WriteString("\n\n")
	if review {
		b.WriteString(a.styles.muted.Render("Approve only commands you understand. Reject or cancel anything unexpected."))
		b.WriteString("\n\n")
	}
	b.WriteString(rowWithWidths(widths, "", "STATUS", "RISK", "HOST", "SOURCE", "AGE", "COMMAND"))
	if len(commands) == 0 {
		b.WriteString("\n")
		b.WriteString(a.styles.panel.Render("No commands."))
		return b.String()
	}
	for i, rec := range commands {
		marker := " "
		if i == a.selected {
			marker = a.styles.selected.Render(">")
		}
		age := "-"
		if !rec.CreatedAt.IsZero() {
			age = time.Since(rec.CreatedAt).Round(time.Second).String()
		}
		status := a.styles.status(string(rec.Status)).Render(compactStatus(rec.Status))
		risk := a.styles.risk(rec.Risk).Render(strings.ToUpper(string(rec.Risk)))
		b.WriteString(rowWithWidths(widths, marker, status, risk, rec.Host, rec.Source, age, rec.Command))
	}
	return b.String()
}

func commandDetail(rec model.CommandRecord, s styles) string {
	stdout := tail(rec.Stdout, 8)
	stderr := tail(rec.Stderr, 8)
	lines := []string{
		s.title.Render("Command Detail"),
		"",
		"ID: " + rec.ID,
		"Host: " + rec.Host,
		"User: " + emptyDash(rec.RemoteUser),
		"Source: " + emptyDash(rec.Source),
		"Workspace: " + emptyDash(rec.ClientCWD),
		"Status: " + s.status(string(rec.Status)).Render(string(rec.Status)),
		"Risk: " + s.risk(rec.Risk).Render(strings.ToUpper(string(rec.Risk))),
		"Rule: " + emptyDash(rec.PolicyRule),
		"Pattern: " + emptyDash(rec.MatchedPattern),
		"Reason: " + emptyDash(rec.PolicyReason),
		"",
		s.section.Render("Command"),
		rec.Command,
		"",
		s.section.Render("stdout tail"),
		emptyDash(stdout),
		"",
		s.section.Render("stderr tail"),
		emptyDash(stderr),
	}
	return s.panel.Render(strings.Join(lines, "\n"))
}

func (a app) visibleCommands() []model.CommandRecord {
	all := make([]model.CommandRecord, 0, len(a.commands))
	for _, rec := range a.commands {
		if a.page == pageReview && rec.Status != model.StatusPendingApproval {
			continue
		}
		if !matchesFilter(rec, a.filter) {
			continue
		}
		all = append(all, rec)
	}
	sort.Slice(all, func(i, j int) bool {
		if a.page == pageReview {
			ri := riskRank(all[i].Risk)
			rj := riskRank(all[j].Risk)
			if ri != rj {
				return ri > rj
			}
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})
	return all
}

func matchesFilter(rec model.CommandRecord, filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{rec.ID, rec.Host, rec.RemoteUser, rec.Source, rec.ClientCWD, rec.Command, string(rec.Status), string(rec.Risk), rec.PolicyRule}, "\n"))
	return strings.Contains(haystack, filter)
}

func (a *app) selectedCommand() (model.CommandRecord, bool) {
	commands := a.visibleCommands()
	if len(commands) == 0 {
		return model.CommandRecord{}, false
	}
	a.clampSelection()
	return commands[a.selected], true
}

func (a app) selectedOrZero(commands []model.CommandRecord) model.CommandRecord {
	if len(commands) == 0 {
		return model.CommandRecord{}
	}
	idx := a.selected
	if idx < 0 {
		idx = 0
	}
	if idx >= len(commands) {
		idx = len(commands) - 1
	}
	return commands[idx]
}

func (a *app) clampSelection() {
	maxIdx := 0
	switch a.page {
	case pageConnections:
		maxIdx = len(a.connections) - 1
	default:
		maxIdx = len(a.visibleCommands()) - 1
	}
	if maxIdx < 0 {
		a.selected = 0
		return
	}
	if a.selected > maxIdx {
		a.selected = maxIdx
	}
	if a.selected < 0 {
		a.selected = 0
	}
}

func nextMode(mode string) string {
	switch mode {
	case config.ModeSensitive:
		return config.ModeApproval
	case config.ModeApproval:
		return config.ModeAuto
	default:
		return config.ModeSensitive
	}
}

func row(cols ...string) string {
	widths := []int{2, 14, 10, 16, 14, 9, 80, 1}
	return rowWithWidths(widths, cols...)
}

func rowWithWidths(widths []int, cols ...string) string {
	var b strings.Builder
	for i, col := range cols {
		if i >= len(widths) {
			break
		}
		b.WriteString(pad(truncate(col, widths[i]), widths[i]))
		if i < len(cols)-1 {
			b.WriteByte(' ')
		}
	}
	b.WriteByte('\n')
	return b.String()
}

func compactStatus(status model.Status) string {
	if status == model.StatusPendingApproval {
		return "PENDING"
	}
	return string(status)
}

func riskRank(risk model.Risk) int {
	switch risk {
	case model.RiskCritical:
		return 4
	case model.RiskHigh:
		return 3
	case model.RiskMedium:
		return 2
	case model.RiskLow:
		return 1
	default:
		return 0
	}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}

func truncate(s string, maxLen int) string {
	s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
	if maxLen <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return ansi.Truncate(s, maxLen, "")
	}
	return ansi.Truncate(s, maxLen, "...")
}

func pad(s string, width int) string {
	visible := ansi.StringWidth(s)
	if visible >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visible)
}

func commandTableWidths(width int) []int {
	widths := []int{2, 14, 10, 16, 14, 9, 80}
	if width <= 0 {
		return widths
	}

	gaps := len(widths) - 1
	available := width - gaps
	if available < len(widths) {
		available = len(widths)
	}
	current := 0
	for _, w := range widths {
		current += w
	}
	if current < available {
		widths[len(widths)-1] += available - current
		return widths
	}

	mins := []int{1, 6, 4, 6, 4, 4, 4}
	shrinkOrder := []int{6, 4, 3, 1, 5, 2, 0}
	for current > available {
		shrunk := false
		for _, idx := range shrinkOrder {
			if widths[idx] > mins[idx] {
				widths[idx]--
				current--
				shrunk = true
				break
			}
		}
		if !shrunk {
			break
		}
	}
	return widths
}

func tail(s string, lines int) string {
	if s == "" {
		return ""
	}
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) <= lines {
		return strings.Join(parts, "\n")
	}
	return strings.Join(parts[len(parts)-lines:], "\n")
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
