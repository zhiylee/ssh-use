package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/zhiylee/ssh-use/internal/client"
	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/model"
	"github.com/zhiylee/ssh-use/internal/paths"
	"github.com/zhiylee/ssh-use/internal/protocol"
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
	safe             bool
	page             page
	selected         int
	selectedID       string
	width            int
	height           int
	commands         map[string]model.CommandRecord
	connections      []model.ConnectionStatus
	rules            []model.PolicyRuleView
	paused           bool
	mode             string
	message          string
	help             bool
	detail           bool
	filter           string
	filterDraft      string
	inputMode        inputMode
	confirm          *confirmation
	confirmRisk      map[model.Risk]struct{}
	viewport         viewport.Model
	sub              *subscription
	styles           styles
	theme            string
	focusPending     bool
	bellPending      bool
	streamHealthy    bool
	streamAlive      bool
	busy             bool
	requestSeq       int
	refreshSeq       int
	refreshing       bool
	refreshQueued    bool
	lastRevision     uint64
	configGeneration uint64
	config           *config.Config
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
type bellMsg struct{}
type requestResult struct {
	requestID int
	message   protocol.Message
	err       error
}
type snapshotResult struct {
	refreshID int
	message   protocol.Message
	err       error
}

func Run(args []string) int {
	safe := false
	for _, arg := range args {
		switch arg {
		case "--safe":
			safe = true
		default:
			fmt.Fprintf(os.Stderr, "ssh-use: unknown tui arg %q\n", arg)
			return 2
		}
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh-use: %v\n", err)
		return 1
	}
	if err := ensureDaemonFn(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "ssh-use: %v\n", err)
		return 1
	}
	sub, err := newSubscription()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh-use: %v\n", err)
		return 1
	}
	defer sub.conn.Close()

	m := newAppWithConfig(safe, sub, cfg)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "ssh-use tui: %v\n", err)
		return 1
	}
	return 0
}

func newApp(safe bool, sub *subscription, confirmRisks []string) app {
	cfg := config.Default()
	cfg.Approval.ConfirmRisks = confirmRisks
	return newAppWithConfig(safe, sub, cfg)
}

func newAppWithConfig(safe bool, sub *subscription, cfg *config.Config) app {
	vp := viewport.New(80, 20)
	vp.KeyMap.Up.SetKeys("pgup", "ctrl+u")
	vp.KeyMap.Down.SetKeys("pgdown", "ctrl+d")
	confirmRisk := make(map[model.Risk]struct{}, len(cfg.Approval.ConfirmRisks))
	for _, risk := range cfg.Approval.ConfirmRisks {
		confirmRisk[model.Risk(risk)] = struct{}{}
	}
	focusPending := cfg.TUI.FocusPending != nil && *cfg.TUI.FocusPending
	return app{
		safe:          safe,
		page:          pageReview,
		commands:      map[string]model.CommandRecord{},
		mode:          cfg.Policy.Mode,
		confirmRisk:   confirmRisk,
		viewport:      vp,
		sub:           sub,
		styles:        newStylesFor(cfg.TUI.Theme, safe),
		theme:         cfg.TUI.Theme,
		focusPending:  focusPending,
		bellPending:   cfg.TUI.BellOnPending,
		streamHealthy: true,
		streamAlive:   true,
		config:        cfg,
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
		a.resizeViewport()
		return a, nil
	case tea.KeyMsg:
		cmd := a.handleKey(msg)
		return a, cmd
	case daemonMsg:
		cmd := a.apply(protocol.Message(msg))
		a.restoreSelection()
		return a, tea.Batch(a.waitDaemonMsg(), cmd)
	case daemonErr:
		a.streamAlive = false
		a.streamHealthy = false
		if msg == nil || error(msg) == io.EOF {
			a.message = "live updates disconnected — restart the TUI to reconnect"
		} else {
			a.message = "live updates disconnected: " + error(msg).Error()
		}
		return a, nil
	case requestResult:
		if msg.requestID != a.requestSeq {
			return a, nil
		}
		a.busy = false
		if a.confirm != nil {
			a.confirm.sending = false
		}
		if msg.err != nil {
			if errors.Is(msg.err, context.DeadlineExceeded) || errors.Is(msg.err, os.ErrDeadlineExceeded) {
				a.message = "request timed out — outcome unknown; refreshing state"
				return a, a.startSnapshotRefresh()
			}
			a.message = "request failed: " + msg.err.Error()
			return a, nil
		}
		if !msg.message.OK && msg.message.Error != "" {
			a.message = "request failed: " + msg.message.Error
			return a, nil
		}
		if msg.message.RuntimeSettings != nil {
			a.applyRuntimeSettings(msg.message.RuntimeSettings)
		} else if msg.message.Mode != "" {
			a.mode = msg.message.Mode
		}
		if a.confirm != nil {
			a.message = a.confirm.action + " accepted"
			a.confirm = nil
		} else {
			a.message = "request accepted"
		}
		return a, nil
	case bellMsg:
		return a, nil
	case snapshotResult:
		if msg.refreshID != a.refreshSeq {
			return a, nil
		}
		a.refreshing = false
		if msg.err != nil {
			a.message = "refresh failed: " + msg.err.Error()
		} else {
			a.applyRefreshSnapshot(msg.message)
			a.restoreSelection()
		}
		if a.refreshQueued {
			a.refreshQueued = false
			return a, a.startSnapshotRefresh()
		}
		return a, nil
	}
	var cmd tea.Cmd
	a.viewport, cmd = a.viewport.Update(msg)
	return a, cmd
}

func (a *app) resizeViewport() {
	headerHeight := lipgloss.Height(a.header())
	footerHeight := lipgloss.Height(a.footer())
	a.viewport.Width = max(1, a.width)
	a.viewport.Height = max(1, a.height-headerHeight-footerHeight)
}

func (a app) View() string {
	if a.width == 0 {
		return "Starting ssh-use…"
	}
	if a.width < 40 || a.height < 14 {
		return a.styles.panel.Render(fmt.Sprintf("Terminal too small\n\nCurrent: %d×%d\nRequired: 40×14", a.width, a.height))
	}
	content := a.content()
	a.viewport.SetContent(content)
	header := a.header()
	footer := a.footer()
	return lipgloss.JoinVertical(lipgloss.Left, header, a.viewport.View(), footer)
}

func (a *app) syncViewportContent() {
	a.viewport.SetContent(a.content())
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
	case "esc":
		if a.help || a.detail {
			a.help = false
			a.detail = false
			a.viewport.GotoTop()
		} else if a.filter != "" {
			a.filter = ""
			a.message = "filter cleared"
			a.restoreSelection()
		}
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
		a.moveSelection(1)
	case "k", "up":
		a.moveSelection(-1)
	case "g", "home":
		a.selected = 0
		a.rememberSelection()
		a.viewport.GotoTop()
	case "G", "end":
		a.selected = a.itemCount() - 1
		a.clampSelection()
		a.rememberSelection()
		a.syncViewportContent()
		a.viewport.GotoBottom()
	case "pgup", "ctrl+u":
		a.syncViewportContent()
		a.viewport.HalfViewUp()
	case "pgdown", "ctrl+d":
		a.syncViewportContent()
		a.viewport.HalfViewDown()
	case "enter":
		if a.help {
			a.help = false
			a.viewport.GotoTop()
		} else if a.page == pageReview || a.page == pageActivity || a.page == pageConnections {
			a.detail = !a.detail
			a.viewport.GotoTop()
		}
	case "?":
		a.help = !a.help
		a.detail = false
		a.viewport.GotoTop()
	case "/":
		if a.page == pageReview || a.page == pageActivity {
			a.inputMode = inputFilter
			a.filterDraft = a.filter
			a.message = "Filter: " + a.filterDraft + "  • Enter apply  Esc cancel  Ctrl+U clear"
		}
	case "a":
		if a.page == pageReview {
			if a.busy {
				a.message = "wait for the current request to finish"
			} else {
				return a.decide("approve")
			}
		}
	case "r":
		if a.page == pageReview {
			if a.busy {
				a.message = "wait for the current request to finish"
			} else {
				return a.decide("reject")
			}
		}
	case "x":
		if a.page == pageReview || a.page == pageActivity {
			if a.busy {
				a.message = "wait for the current request to finish"
			} else {
				a.confirmCancel()
			}
		}
	case "p":
		return a.togglePause()
	case "!":
		if a.busy {
			a.message = "wait for the current request to finish"
		} else {
			a.confirmEmergencyStop()
		}
	case "m":
		if a.busy {
			a.message = "wait for the current request to finish"
		} else {
			a.confirmModeChange()
		}
	case "d":
		if a.page == pageConnections {
			return a.closeSelectedConnection()
		}
	case "D":
		if a.page == pageConnections {
			if a.busy {
				a.message = "wait for the current request to finish"
			} else {
				a.confirmCloseIdleConnections()
			}
		}
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
	case "esc", "n", "N", "q":
		a.message = "cancelled " + a.confirm.action
		a.confirm = nil
	case "enter":
		if a.busy {
			a.message = "wait for the current request to finish"
			return nil
		}
		payload := a.confirm.payload
		a.message = a.confirm.action + " in progress…"
		a.confirm.sending = true
		return a.startRequest(payload)
	}
	return nil
}

func (a *app) handleFilterKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		a.inputMode = inputNone
		a.filter = strings.TrimSpace(a.filterDraft)
	case "esc":
		a.inputMode = inputNone
		a.filterDraft = a.filter
		a.message = "filter unchanged"
		return nil
	case "backspace", "ctrl+h":
		if len(a.filterDraft) > 0 {
			_, size := utf8.DecodeLastRuneInString(a.filterDraft)
			a.filterDraft = a.filterDraft[:len(a.filterDraft)-size]
		}
	case "ctrl+u":
		a.filterDraft = ""
	default:
		for _, r := range msg.Runes {
			if unicode.IsPrint(r) {
				a.filterDraft += string(r)
			}
		}
	}
	a.selected = 0
	a.selectedID = ""
	a.message = "Filter: " + a.filterDraft + "  • Enter apply  Esc cancel  Ctrl+U clear"
	if a.inputMode == inputNone {
		if a.filter == "" {
			a.message = "filter cleared"
		} else {
			a.message = "filter active: " + a.filter
		}
	}
	return nil
}

func (a *app) setPage(p page) {
	a.page = p
	a.selected = 0
	a.selectedID = ""
	a.help = false
	a.detail = false
	a.message = ""
	a.viewport.GotoTop()
	a.rememberSelection()
}

func (a *app) applyRefreshSnapshot(msg protocol.Message) {
	if msg.Revision > a.lastRevision {
		a.lastRevision = msg.Revision
	}
	a.streamHealthy = a.streamAlive
	if msg.RuntimeSettings != nil {
		a.applyRuntimeSettings(msg.RuntimeSettings)
	} else if msg.Mode != "" {
		a.mode = msg.Mode
	}
	a.paused = msg.Paused
	a.connections = msg.Connections
	a.rules = msg.PolicyRules
	for _, rec := range msg.Commands {
		if _, exists := a.commands[rec.ID]; !exists {
			a.commands[rec.ID] = rec
		}
	}
}

func (a *app) applyRuntimeSettings(settings *protocol.RuntimeSettings) {
	if settings == nil {
		return
	}
	if settings.Generation > 0 {
		if settings.Generation < a.configGeneration {
			return
		}
		a.configGeneration = settings.Generation
	}
	cfg := a.config.Clone()
	if cfg == nil {
		cfg = config.Default()
	}
	builtinRules := settings.BuiltinRules
	redactSecrets := settings.RedactSecrets
	focusPending := settings.FocusPending
	cfg.Policy.Mode = settings.Mode
	cfg.Policy.DefaultAction = settings.PolicyDefaultAction
	cfg.Policy.BuiltinRules = &builtinRules
	cfg.Audit.StoreOutput = settings.AuditStoreOutput
	cfg.Audit.RetentionDays = settings.AuditRetentionDays
	cfg.Audit.RedactSecrets = &redactSecrets
	cfg.Approval.ConfirmRisks = append([]string(nil), settings.ConfirmRisks...)
	cfg.TUI.Theme = settings.Theme
	cfg.TUI.FocusPending = &focusPending
	cfg.TUI.BellOnPending = settings.BellOnPending
	a.config = cfg
	a.mode = settings.Mode
	a.confirmRisk = make(map[model.Risk]struct{}, len(settings.ConfirmRisks))
	for _, risk := range settings.ConfirmRisks {
		a.confirmRisk[model.Risk(risk)] = struct{}{}
	}
	a.focusPending = settings.FocusPending
	a.bellPending = settings.BellOnPending
	a.theme = settings.Theme
	a.styles = newStylesFor(settings.Theme, a.safe)
}

func (a *app) apply(msg protocol.Message) tea.Cmd {
	var recovery tea.Cmd
	if msg.Revision > 0 {
		if a.lastRevision > 0 && msg.Revision != a.lastRevision+1 {
			a.streamHealthy = false
			a.message = "live update gap detected — refreshing state"
			if !a.refreshing {
				recovery = a.startSnapshotRefresh()
			} else {
				a.refreshQueued = true
			}
		}
		a.lastRevision = msg.Revision
	}
	if msg.Type == "ui.error" {
		a.message = "error: " + msg.Error
		return recovery
	}
	switch msg.Type {
	case "snapshot":
		a.paused = msg.Paused
		if msg.RuntimeSettings != nil {
			a.applyRuntimeSettings(msg.RuntimeSettings)
		} else if msg.Mode != "" {
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
		if msg.RuntimeSettings != nil {
			a.applyRuntimeSettings(msg.RuntimeSettings)
		} else if msg.Mode != "" {
			a.mode = msg.Mode
		}
	case "config.reloaded":
		if msg.RuntimeSettings != nil {
			a.applyRuntimeSettings(msg.RuntimeSettings)
		} else if msg.Mode != "" {
			a.mode = msg.Mode
		}
		a.message = "configuration reloaded"
		if recovery == nil {
			if a.refreshing {
				a.refreshQueued = true
			} else {
				recovery = a.startSnapshotRefresh()
			}
		}
	case "config.reload_failed":
		a.message = "configuration reload failed: " + msg.Error
	case "connection.updated":
		if a.refreshing {
			a.refreshQueued = true
			return recovery
		}
		if recovery == nil {
			recovery = a.startSnapshotRefresh()
		}
	default:
		if msg.Record != nil {
			previous, existed := a.commands[msg.Record.ID]
			a.commands[msg.Record.ID] = *msg.Record
			becamePending := msg.Record.Status == model.StatusPendingApproval && (!existed || previous.Status != model.StatusPendingApproval)
			if becamePending {
				if a.focusPending {
					a.page = pageReview
					a.filter = ""
					a.filterDraft = ""
					a.inputMode = inputNone
					a.selectedID = msg.Record.ID
					a.detail = false
					a.help = false
					a.message = "new approval request: " + shortID(msg.Record.ID)
				}
				if a.bellPending {
					bell := func() tea.Msg {
						_, _ = os.Stderr.WriteString("\a")
						return bellMsg{}
					}
					recovery = tea.Batch(recovery, bell)
				}
			}
		}
	}
	return recovery
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
	return a.startRequest(protocol.Message{Type: "approval.decide", ID: rec.ID, Decision: decision})
}

func (a *app) confirmApproval(rec model.CommandRecord) {
	body := fmt.Sprintf("Command: %s\nHost: %s  User: %s\nRisk: %s  Rule: %s\n\n%s\n\n[Enter] Approve exact command    [Esc/q] Go back", shortID(rec.ID), safeText(rec.Host), emptyDash(safeText(rec.RemoteUser)), strings.ToUpper(string(rec.Risk)), emptyDash(safeText(rec.PolicyRule)), wrapText(safeText(displayCommand(rec)), dialogWidth(a.width)-6))
	a.confirm = &confirmation{title: "APPROVE COMMAND?", body: body, action: "approve", payload: protocol.Message{Type: "approval.decide", ID: rec.ID, Decision: "approve"}}
	a.message = "confirm approve " + shortID(rec.ID)
}

func (a *app) cancelSelected() tea.Cmd {
	rec, ok := a.selectedCommand()
	if !ok {
		a.message = "no command selected"
		return nil
	}
	a.message = "cancel requested for " + shortID(rec.ID)
	return a.startRequest(protocol.Message{Type: "command.cancel", ID: rec.ID})
}

func (a *app) confirmCancel() {
	rec, ok := a.selectedCommand()
	if !ok {
		a.message = "no command selected"
		return
	}
	if !rec.Status.Cancellable() {
		a.message = "selected command has already finished"
		return
	}
	body := fmt.Sprintf("Command: %s\nHost: %s\nStatus: %s\n\n%s\n\n[Enter] Cancel command    [Esc/q] Go back", shortID(rec.ID), safeText(rec.Host), rec.Status, wrapText(safeText(displayCommand(rec)), dialogWidth(a.width)-6))
	a.confirm = &confirmation{title: "Cancel command?", body: body, action: "cancel", payload: protocol.Message{Type: "command.cancel", ID: rec.ID}}
	a.message = "confirm cancel"
}

func (a *app) togglePause() tea.Cmd {
	a.message = "pause state change in progress…"
	return a.startRequest(protocol.Message{Type: "daemon.pause", Paused: !a.paused})
}

func (a *app) emergencyStop() tea.Cmd {
	a.message = "emergency stop in progress…"
	return a.startRequest(protocol.Message{Type: "daemon.emergency_stop"})
}

func (a *app) confirmEmergencyStop() {
	a.confirm = &confirmation{
		title:   "Emergency stop?",
		body:    "This pauses new commands and cancels pending and queued commands. Running commands continue and must be cancelled individually from Activity.\n\n[Enter] Emergency stop    [Esc/q] Go back",
		action:  "emergency stop",
		payload: protocol.Message{Type: "daemon.emergency_stop"},
	}
	a.message = "confirm emergency stop"
}

func (a *app) cycleMode() tea.Cmd {
	next := nextMode(a.mode)
	a.message = "mode change in progress: " + next
	return a.startRequest(protocol.Message{Type: "policy.set_mode", Mode: next})
}

func (a *app) confirmModeChange() {
	next := nextMode(a.mode)
	body := fmt.Sprintf("Switch execution mode from %s to %s?\n\n%s\nBlock rules always apply.\n\n[Enter] Switch mode    [Esc/q] Go back", strings.ToUpper(a.mode), strings.ToUpper(next), a.modeDescription(next))
	a.confirm = &confirmation{title: "Switch execution mode?", body: body, action: "mode change", payload: protocol.Message{Type: "policy.set_mode", Mode: next}}
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
	a.message = "disconnect in progress for " + conn.Host
	return a.startRequest(protocol.Message{Type: "connections.close_host", Host: conn.Host})
}

func (a *app) closeIdleConnections() tea.Cmd {
	a.message = "closing idle connections…"
	return a.startRequest(protocol.Message{Type: "connections.close_idle"})
}

func (a *app) confirmCloseIdleConnections() {
	a.confirm = &confirmation{
		title:   "Close idle connections?",
		body:    "All idle pooled SSH connections will be closed. Active sessions are not affected.\n\n[Enter] Close idle connections    [Esc/q] Go back",
		action:  "close idle connections",
		payload: protocol.Message{Type: "connections.close_idle"},
	}
	a.message = "confirm closing idle connections"
}

func (a *app) startRequest(req protocol.Message) tea.Cmd {
	if a.busy {
		a.message = "another request is already in progress"
		return nil
	}
	a.busy = true
	a.requestSeq++
	requestID := a.requestSeq
	return func() tea.Msg {
		resp, err := requestWithTimeout(req)
		return requestResult{requestID: requestID, message: resp, err: err}
	}
}

func (a *app) startSnapshotRefresh() tea.Cmd {
	a.refreshSeq++
	a.refreshing = true
	return requestSnapshotCmd(a.refreshSeq)
}

func requestSnapshotCmd(refreshID int) tea.Cmd {
	return func() tea.Msg {
		resp, err := requestWithTimeout(protocol.Message{Type: "snapshot"})
		return snapshotResult{refreshID: refreshID, message: resp, err: err}
	}
}

func requestWithTimeout(req protocol.Message) (protocol.Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return requestFn(ctx, req)
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
	status := a.styles.ok.Render("● LIVE")
	if !a.streamHealthy {
		status = a.styles.danger.Render("● STALE")
	} else if a.paused {
		status = a.styles.warn.Render("● PAUSED")
	}
	tabs := []string{"1 Review", "2 Activity", "3 Connections", "4 Policy", "5 Settings"}
	if a.width < 78 {
		tabs = []string{"1 Review", "2 Activity", "3 SSH", "4 Policy", "5 More"}
	}
	for i := range tabs {
		if page(i+1) == a.page {
			tabs[i] = a.styles.tabActive.Render(tabs[i])
		} else {
			tabs[i] = a.styles.tab.Render(tabs[i])
		}
	}
	line := fmt.Sprintf("ssh-use  %s  %s", status, a.styles.mode(a.mode).Render(strings.ToUpper(a.mode)))
	counts := fmt.Sprintf("Review %d  Running %d  Attention %d  SSH %d", pending, running, failed, len(a.connections))
	if a.width >= 92 {
		line += "  " + counts
	}
	return a.styles.header.Width(max(1, a.width-2)).Render(truncate(line, max(1, a.width-2))) + "\n" + truncate(strings.Join(tabs, " "), a.width)
}

func (a *app) footer() string {
	hints := "↑↓ navigate  Enter details  / filter  ? help  q quit"
	switch a.page {
	case pageReview:
		hints = "a approve  r reject  x cancel  ↑↓ navigate  Enter details  / filter  ? help"
	case pageActivity:
		hints = "x cancel active  ↑↓ navigate  Enter details  / filter  ? help"
	case pageConnections:
		hints = "d disconnect  D close idle  ↑↓ navigate  Enter details  ? help"
	case pagePolicy, pageSettings:
		hints = "p pause  m mode  ! emergency stop  ? help  q quit"
	}
	if a.help || a.detail {
		hints = "Esc/Enter back  PgUp/PgDn scroll  q quit"
	}
	if a.filter != "" && a.inputMode == inputNone {
		hints = "FILTER “" + a.filter + "”  • Esc clear  • " + hints
	}
	if a.message != "" {
		hints = a.message + "  │  " + hints
	}
	return a.styles.footer.Width(max(1, a.width-2)).Render(truncate(hints, max(1, a.width-2)))
}

func (a app) content() string {
	if a.confirm != nil {
		body := a.confirm.body
		if a.confirm.sending {
			body += "\n\nWorking… Ctrl+C closes the TUI but does not undo an accepted request."
		}
		return a.centerDialog(a.styles.dialog.Render(a.styles.title.Render(a.confirm.title) + "\n\n" + body))
	}
	if a.help {
		return a.helpView()
	}
	if a.detail {
		switch a.page {
		case pageReview, pageActivity:
			if rec, ok := a.selectedCommand(); ok {
				return commandDetail(rec, a.styles, max(24, a.width-4))
			}
		case pageConnections:
			return a.connectionDetail()
		}
	}
	switch a.page {
	case pageReview:
		return a.reviewView()
	case pageActivity:
		return a.activityView()
	case pageConnections:
		return a.connectionsView()
	case pagePolicy:
		return a.policyView()
	case pageSettings:
		return a.settingsView()
	default:
		return ""
	}
}

func (a app) centerDialog(dialog string) string {
	width := lipgloss.Width(dialog)
	left := max(0, (a.width-width)/2)
	top := max(0, (a.viewport.Height-lipgloss.Height(dialog))/3)
	return strings.Repeat("\n", top) + lipgloss.NewStyle().MarginLeft(left).Render(dialog)
}

func (a app) reviewView() string {
	commands := a.visibleCommands()
	if len(commands) == 0 {
		if a.filter != "" && a.pendingCount() > 0 {
			return a.emptyState("No matching approvals", fmt.Sprintf("%d approval request(s) are hidden by filter “%s”.\n\nPress Esc to clear the filter.", a.pendingCount(), safeText(a.filter)))
		}
		return a.emptyState("Review queue is clear", "No commands are waiting for approval.\n\nNew sensitive commands will appear here automatically.")
	}
	if a.safe || a.width < 120 || a.height < 32 {
		return a.commandTable("Pending Review", commands, true, a.width)
	}
	tableWidth := a.width*3/5 - 2
	detailWidth := a.width - tableWidth - 3
	left := lipgloss.NewStyle().Width(tableWidth).Render(a.commandTable("Pending Review", commands, true, tableWidth))
	right := lipgloss.NewStyle().Width(detailWidth).Render(commandDetail(a.selectedOrZero(commands), a.styles, detailWidth))
	return lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
}

func (a app) activityView() string {
	commands := a.visibleCommands()
	if len(commands) == 0 && a.filter != "" && len(a.commands) > 0 {
		return a.emptyState("No matching activity", "No command matches the active filter.\n\nPress Esc to clear it.")
	}
	return a.commandTable("Activity", commands, false, a.width)
}

func (a app) emptyState(title, body string) string {
	return a.styles.panel.Width(max(20, min(a.width-6, 64))).Render(a.styles.title.Render(title) + "\n\n" + body)
}

func (a app) connectionsView() string {
	var b strings.Builder
	b.WriteString(a.styles.title.Render("Connections"))
	b.WriteString("\n\n")
	b.WriteString(a.styles.muted.Render("Pooled SSH connections • Enter details • d close selected idle • D close all idle"))
	b.WriteString("\n\n")
	if len(a.connections) == 0 {
		b.WriteString(a.emptyState("No pooled connections", "Connections appear here after the first SSH command."))
		return b.String()
	}
	widths := connectionTableWidths(a.width)
	b.WriteString(rowWithWidths(widths, "", "HOST", "STATUS", "USER", "SESS", "IDLE", "LAST COMMAND"))
	for i, conn := range a.connections {
		marker := " "
		if i == a.selected {
			marker = a.styles.selected.Render(">")
		}
		idle := "-"
		if !conn.LastUsed.IsZero() {
			idle = time.Since(conn.LastUsed).Round(time.Second).String()
		}
		b.WriteString(rowWithWidths(widths, marker, safeText(conn.Host), a.styles.status(conn.Status).Render(safeText(conn.Status)), safeText(conn.User), fmt.Sprint(conn.OpenSessions), idle, safeText(conn.LastCommand)))
	}
	return b.String()
}

func (a app) connectionDetail() string {
	if len(a.connections) == 0 {
		return a.emptyState("No connection selected", "Press Esc to return.")
	}
	idx := min(max(0, a.selected), len(a.connections)-1)
	conn := a.connections[idx]
	lastUsed := "-"
	if !conn.LastUsed.IsZero() {
		lastUsed = conn.LastUsed.Local().Format(time.DateTime)
	}
	connected := "-"
	if !conn.ConnectedAt.IsZero() {
		connected = conn.ConnectedAt.Local().Format(time.DateTime)
	}
	lines := []string{
		a.styles.title.Render("Connection Detail"), "",
		"Host: " + safeText(conn.Host),
		"Status: " + a.styles.status(conn.Status).Render(safeText(conn.Status)),
		"User: " + emptyDash(safeText(conn.User)),
		"Address: " + emptyDash(safeText(conn.Addr)),
		fmt.Sprintf("Open sessions: %d", conn.OpenSessions),
		"Connected: " + connected,
		"Last used: " + lastUsed, "",
		a.styles.section.Render("Last command"),
		wrapText(emptyDash(safeText(conn.LastCommand)), max(20, a.width-10)), "",
		"[Esc/Enter] Back    [d] Disconnect when idle",
	}
	return a.styles.panel.Width(max(20, min(a.width-6, 90))).Render(strings.Join(lines, "\n"))
}

func (a app) policyView() string {
	var b strings.Builder
	b.WriteString(a.styles.title.Render("Policy"))
	b.WriteString("\n\n")
	defaultAction := "-"
	builtin := "-"
	if a.config != nil {
		defaultAction = a.config.Policy.DefaultAction
		if a.config.Policy.BuiltinRules != nil {
			builtin = fmt.Sprint(a.config.BuiltinRulesEnabled())
		}
	}
	b.WriteString(fmt.Sprintf("Mode %s  •  Default %s  •  Built-in rules %s\n\n", a.styles.mode(a.mode).Render(strings.ToUpper(a.mode)), strings.ToUpper(defaultAction), builtin))
	if len(a.rules) == 0 {
		b.WriteString(a.emptyState("No policy rules", "No custom or built-in policy rules are currently loaded."))
		return b.String()
	}
	widths := policyTableWidths(a.width)
	b.WriteString(rowWithWidths(widths, "RULE", "ACTION", "RISK", "MATCHES", "PATTERNS"))
	for _, rule := range a.rules {
		patterns := strings.Join(rule.Patterns, "; ")
		b.WriteString(rowWithWidths(widths, safeText(rule.Name), strings.ToUpper(string(rule.Action)), a.styles.risk(rule.Risk).Render(strings.ToUpper(string(rule.Risk))), fmt.Sprint(rule.Matches), safeText(patterns)))
	}
	return b.String()
}

func (a app) settingsView() string {
	paused := a.styles.ok.Render("NO")
	if a.paused {
		paused = a.styles.danger.Render("YES")
	}
	audit := "-"
	if a.config != nil {
		audit = fmt.Sprintf("%s • %d days • redaction %s", a.config.Audit.StoreOutput, a.config.Audit.RetentionDays, yesNo(a.config.RedactSecrets()))
	}
	items := []string{
		a.styles.section.Render("Runtime"),
		"Execution mode        " + a.styles.mode(a.mode).Render(strings.ToUpper(a.mode)),
		"Paused                " + paused,
		"Live event stream     " + healthLabel(a.streamHealthy, a.styles), "",
		a.styles.section.Render("Interface"),
		fmt.Sprintf("Theme                 %s", strings.ToUpper(a.theme)),
		fmt.Sprintf("Safe layout           %s", yesNo(a.safe)),
		fmt.Sprintf("Focus new approvals   %s", yesNo(a.focusPending)),
		fmt.Sprintf("Bell on approval      %s", yesNo(a.bellPending)), "",
		a.styles.section.Render("Audit"),
		"Output                 " + audit, "",
		a.styles.muted.Render("Configuration: " + safeText(paths.ConfigPath())),
	}
	return a.styles.title.Render("Settings") + "\n\n" + a.styles.panel.Render(strings.Join(items, "\n"))
}

func (a app) helpView() string {
	groups := []string{
		a.styles.title.Render("Keyboard Help"), "",
		a.styles.section.Render("Navigate"),
		"1–5             Switch page",
		"↑/↓ or j/k      Move selection",
		"g / G           First / last item",
		"PgUp / PgDn     Scroll content",
		"Enter           Open or close details",
		"/               Filter Review or Activity",
		"Esc             Back or clear active filter", "",
		a.styles.section.Render("Review & Activity"),
		"a               Approve selected request (Review only)",
		"r               Reject selected request (Review only)",
		"x               Cancel selected active command", "",
		a.styles.section.Render("Controls"),
		"p               Pause or resume new commands",
		"!               Emergency stop",
		"m               Cycle execution mode",
		"d / D           Close selected / all idle connections", "",
		"?               Close help",
		"q / Ctrl+C      Quit TUI; daemon keeps running",
	}
	return a.styles.panel.Width(max(24, min(a.width-6, 74))).Render(strings.Join(groups, "\n"))
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
		b.WriteString(rowWithWidths(widths, marker, status, risk, safeText(rec.Host), safeText(rec.Source), age, safeText(displayCommand(rec))))
	}
	return b.String()
}

func commandDetail(rec model.CommandRecord, s styles, width int) string {
	innerWidth := max(18, min(width-8, 100))
	stdout := safeText(tail(rec.Stdout, 12))
	stderr := safeText(tail(rec.Stderr, 12))
	exitCode := "-"
	if rec.RemoteExitCode != nil {
		exitCode = fmt.Sprint(*rec.RemoteExitCode)
	}
	duration := "-"
	if rec.DurationMS > 0 {
		duration = (time.Duration(rec.DurationMS) * time.Millisecond).Round(time.Millisecond).String()
	}
	lines := []string{
		s.title.Render("Command Detail"), "",
		"ID: " + safeText(rec.ID),
		"Target: " + safeText(rec.Host) + "  •  User: " + emptyDash(safeText(rec.RemoteUser)),
		"Source: " + emptyDash(safeText(rec.Source)),
		"Workspace: " + emptyDash(safeText(rec.ClientCWD)),
		"Status: " + s.status(string(rec.Status)).Render(string(rec.Status)) + "  •  Risk: " + s.risk(rec.Risk).Render(strings.ToUpper(string(rec.Risk))),
		"Exit: " + exitCode + "  •  Duration: " + duration,
		"Policy: " + emptyDash(safeText(rec.PolicyRule)) + "  •  Action: " + emptyDash(string(rec.PolicyAction)),
		"Matched: " + emptyDash(safeText(rec.MatchedPattern)),
		"Reason: " + emptyDash(safeText(rec.PolicyReason)),
	}
	if rec.Error != "" || rec.SSHUseErrorCode != "" {
		lines = append(lines, "Error: "+emptyDash(safeText(rec.SSHUseErrorCode))+" "+safeText(rec.Error))
	}
	lines = append(lines, "", s.section.Render("Command"), wrapText(safeText(displayCommand(rec)), innerWidth))
	stdoutTitle := "stdout tail"
	if rec.StdoutTruncated {
		stdoutTitle += " • truncated"
	}
	stderrTitle := "stderr tail"
	if rec.StderrTruncated {
		stderrTitle += " • truncated"
	}
	lines = append(lines, "", s.section.Render(stdoutTitle), wrapText(emptyDash(stdout), innerWidth), "", s.section.Render(stderrTitle), wrapText(emptyDash(stderr), innerWidth), "", "[Esc/Enter] Back    PgUp/PgDn scroll")
	return s.panel.Width(max(20, min(width-4, 106))).Render(strings.Join(lines, "\n"))
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
			if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
				return all[i].CreatedAt.Before(all[j].CreatedAt)
			}
			return all[i].ID < all[j].ID
		}
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID < all[j].ID
	})
	return all
}

func matchesFilter(rec model.CommandRecord, filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{rec.ID, rec.Host, rec.RemoteUser, rec.Source, rec.ClientCWD, rec.Command, rec.DisplayCommand, string(rec.Status), string(rec.Risk), rec.PolicyRule}, "\n"))
	return strings.Contains(haystack, filter)
}

func (a *app) selectedCommand() (model.CommandRecord, bool) {
	commands := a.visibleCommands()
	if len(commands) == 0 {
		return model.CommandRecord{}, false
	}
	a.restoreSelection()
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
	maxIdx := a.itemCount() - 1
	if maxIdx < 0 {
		a.selected = 0
		a.selectedID = ""
		return
	}
	if a.selected > maxIdx {
		a.selected = maxIdx
	}
	if a.selected < 0 {
		a.selected = 0
	}
}

func (a *app) itemCount() int {
	if a.page == pageConnections {
		return len(a.connections)
	}
	return len(a.visibleCommands())
}

func (a *app) rememberSelection() {
	a.clampSelection()
	if a.page == pageConnections {
		if len(a.connections) > 0 {
			a.selectedID = a.connections[a.selected].Host
		}
		return
	}
	commands := a.visibleCommands()
	if len(commands) > 0 {
		a.selectedID = commands[a.selected].ID
	}
}

func (a *app) restoreSelection() {
	if a.selectedID != "" {
		if a.page == pageConnections {
			for i, conn := range a.connections {
				if conn.Host == a.selectedID {
					a.selected = i
					return
				}
			}
		} else {
			for i, rec := range a.visibleCommands() {
				if rec.ID == a.selectedID {
					a.selected = i
					return
				}
			}
		}
	}
	a.clampSelection()
	a.rememberSelection()
}

func (a *app) moveSelection(delta int) {
	a.selected += delta
	a.clampSelection()
	a.rememberSelection()
	if a.selected == 0 {
		a.viewport.GotoTop()
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
	return fitWidths(width, []int{2, 14, 10, 16, 14, 9, 80}, []int{1, 6, 4, 6, 4, 4, 4}, []int{6, 4, 3, 1, 5, 2, 0})
}

func connectionTableWidths(width int) []int {
	return fitWidths(width, []int{2, 18, 12, 12, 6, 10, 64}, []int{1, 6, 6, 4, 3, 3, 5}, []int{6, 1, 2, 5, 3, 4, 0})
}

func policyTableWidths(width int) []int {
	return fitWidths(width, []int{22, 10, 10, 9, 90}, []int{6, 4, 4, 4, 8}, []int{4, 0, 1, 3, 2})
}

func fitWidths(width int, preferred, minimum, shrinkOrder []int) []int {
	widths := append([]int(nil), preferred...)
	if width <= 0 {
		return widths
	}
	available := max(len(widths), width-(len(widths)-1))
	current := 0
	for _, columnWidth := range widths {
		current += columnWidth
	}
	if current < available {
		widths[len(widths)-1] += available - current
		return widths
	}
	for current > available {
		shrunk := false
		for _, idx := range shrinkOrder {
			if widths[idx] > minimum[idx] {
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

func safeText(s string) string {
	s = controlSequencePattern.ReplaceAllStringFunc(s, func(match string) string {
		if match == "\n" || match == "\t" {
			return match
		}
		return ""
	})
	return strings.Map(func(r rune) rune {
		if r >= 0x80 && r <= 0x9f {
			return -1
		}
		return r
	}, s)
}

var controlSequencePattern = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)?|.)|[\x00-\x08\x0b-\x1f\x7f]`)

func wrapText(s string, width int) string {
	if width <= 1 {
		return truncate(s, max(1, width))
	}
	return ansi.Hardwrap(s, width, true)
}

func displayCommand(rec model.CommandRecord) string {
	if strings.TrimSpace(rec.DisplayCommand) != "" {
		return rec.DisplayCommand
	}
	return rec.Command
}

func (a app) modeDescription(mode string) string {
	switch mode {
	case config.ModeApproval:
		return "All commands require explicit approval."
	case config.ModeAuto:
		return "Allowed commands run without approval, including sensitive matches."
	default:
		defaultAction := "allow"
		if a.config != nil && a.config.Policy.DefaultAction != "" {
			defaultAction = a.config.Policy.DefaultAction
		}
		return fmt.Sprintf("Rules decide matched commands; unmatched commands default to %s.", strings.ToUpper(defaultAction))
	}
}

func dialogWidth(width int) int {
	return max(28, min(width-8, 76))
}

func (a app) pendingCount() int {
	count := 0
	for _, rec := range a.commands {
		if rec.Status == model.StatusPendingApproval {
			count++
		}
	}
	return count
}

func healthLabel(healthy bool, s styles) string {
	if healthy {
		return s.ok.Render("LIVE")
	}
	return s.danger.Render("STALE")
}

func yesNo(value bool) string {
	if value {
		return "YES"
	}
	return "NO"
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
