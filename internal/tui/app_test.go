package tui

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/model"
	"github.com/zhiylee/ssh-use/internal/protocol"
)

func TestApplySnapshotAndVisibleCommands(t *testing.T) {
	a := newTestApp()
	now := time.Now()
	a.apply(protocol.Message{
		Type:   "snapshot",
		Mode:   config.ModeApproval,
		Paused: true,
		Commands: []model.CommandRecord{
			{ID: "low", CreatedAt: now.Add(-time.Second), Host: "h", Command: "uptime", Status: model.StatusPendingApproval, Risk: model.RiskLow},
			{ID: "high", CreatedAt: now, Host: "h", Command: "restart", Status: model.StatusPendingApproval, Risk: model.RiskHigh},
			{ID: "done", CreatedAt: now, Host: "h", Command: "done", Status: model.StatusDone, Risk: model.RiskLow},
		},
		Connections: []model.ConnectionStatus{{Host: "h", Status: "CONNECTED"}},
		PolicyRules: []model.PolicyRuleView{{Name: "r"}},
	})
	if !a.paused || a.mode != config.ModeApproval || len(a.connections) != 1 || len(a.rules) != 1 {
		t.Fatalf("snapshot not applied: %#v", a)
	}
	visible := a.visibleCommands()
	if len(visible) != 2 || visible[0].ID != "high" || visible[1].ID != "low" {
		t.Fatalf("review visible = %#v", visible)
	}
	a.page = pageActivity
	visible = a.visibleCommands()
	if len(visible) != 3 || visible[0].ID == "low" {
		t.Fatalf("activity visible = %#v", visible)
	}
}

func TestSelectionClampAndSelectedCommand(t *testing.T) {
	a := newTestApp()
	a.commands["cmd"] = model.CommandRecord{ID: "cmd", Status: model.StatusPendingApproval, Risk: model.RiskMedium}
	a.selected = 10
	rec, ok := a.selectedCommand()
	if !ok || rec.ID != "cmd" || a.selected != 0 {
		t.Fatalf("selected=%#v ok=%v index=%d", rec, ok, a.selected)
	}
	a.page = pageConnections
	a.connections = []model.ConnectionStatus{{Host: "one"}, {Host: "two"}}
	a.selected = 99
	a.clampSelection()
	if a.selected != 1 {
		t.Fatalf("connection selected index=%d", a.selected)
	}
}

func TestRenderHelpers(t *testing.T) {
	if nextMode(config.ModeSensitive) != config.ModeApproval || nextMode(config.ModeApproval) != config.ModeAuto || nextMode(config.ModeAuto) != config.ModeSensitive {
		t.Fatal("nextMode cycle failed")
	}
	if shortID("cmd_123456789") != "23456789" {
		t.Fatal("shortID failed")
	}
	if truncate("abcdef", 4) != "a..." {
		t.Fatal("truncate failed")
	}
	if tail("a\nb\nc", 2) != "b\nc" {
		t.Fatal("tail failed")
	}
	if emptyDash("  ") != "-" || emptyDash("x") != "x" {
		t.Fatal("emptyDash failed")
	}
	if compactStatus(model.StatusPendingApproval) != "PENDING" || compactStatus(model.StatusDone) != "DONE" {
		t.Fatal("compactStatus failed")
	}
}

func TestViewsContainUsefulContent(t *testing.T) {
	a := newTestApp()
	a.width = 140
	a.height = 40
	a.commands["cmd"] = model.CommandRecord{ID: "cmd", Host: "prod", RemoteUser: "root", Source: "test", ClientCWD: "/tmp", Command: "systemctl restart nginx", Status: model.StatusPendingApproval, Risk: model.RiskHigh, PolicyRule: "service_mutation", PolicyReason: "matched"}
	view := a.reviewView()
	for _, want := range []string{"Pending Review", "Command Detail", "systemctl restart nginx", "service_mutation"} {
		if !strings.Contains(view, want) {
			t.Fatalf("review missing %q in %q", want, view)
		}
	}
	a.page = pagePolicy
	a.rules = []model.PolicyRuleView{{Name: "rule", Action: model.ActionApprove, Risk: model.RiskHigh, Patterns: []string{"x"}, Matches: 1}}
	if !strings.Contains(a.policyView(), "rule") {
		t.Fatal("policy view missing rule")
	}
}

func TestActivityViewFitsLongCommands(t *testing.T) {
	a := newTestApp()
	a.width = 100
	a.page = pageActivity
	a.commands["long"] = model.CommandRecord{
		ID:        "long",
		CreatedAt: time.Now(),
		Host:      "host-with-a-very-long-name",
		Source:    "source-with-a-very-long-name",
		Command:   strings.Repeat("printf-super-long-command ", 12),
		Status:    model.StatusDone,
		Risk:      model.RiskLow,
	}

	for _, line := range strings.Split(strings.TrimRight(a.activityView(), "\n"), "\n") {
		if width := ansi.StringWidth(line); width > a.width {
			t.Fatalf("line width %d exceeds view width %d: %q", width, a.width, line)
		}
	}
}

func TestFilterAndConfirmationState(t *testing.T) {
	a := newTestApp()
	a.commands["one"] = model.CommandRecord{ID: "one", Host: "prod", Command: "systemctl restart nginx", Status: model.StatusPendingApproval, Risk: model.RiskHigh}
	a.commands["two"] = model.CommandRecord{ID: "two", Host: "dev", Command: "uptime", Status: model.StatusPendingApproval, Risk: model.RiskLow}
	a.filter = "prod"
	visible := a.visibleCommands()
	if len(visible) != 1 || visible[0].ID != "one" {
		t.Fatalf("visible=%#v", visible)
	}
	if !matchesFilter(visible[0], "restart") || matchesFilter(visible[0], "nope") {
		t.Fatal("matchesFilter failed")
	}
	if cmd := a.decide("approve"); cmd != nil {
		t.Fatalf("high-risk approval should wait for confirmation: %v", cmd)
	}
	if a.confirm == nil || a.confirm.payload.Type != "approval.decide" || a.confirm.payload.Decision != "approve" || !strings.Contains(a.confirm.body, "[Enter] Approve") {
		t.Fatalf("confirm=%#v", a.confirm)
	}
	a.confirm = nil
	a.confirmCancel()
	if a.confirm == nil || a.confirm.payload.Type != "command.cancel" {
		t.Fatalf("cancel confirm=%#v", a.confirm)
	}
	a.confirmEmergencyStop()
	if a.confirm.payload.Type != "daemon.emergency_stop" {
		t.Fatalf("emergency confirm=%#v", a.confirm)
	}
	a.confirmModeChange()
	if a.confirm.payload.Type != "policy.set_mode" {
		t.Fatalf("mode confirm=%#v", a.confirm)
	}
}

func TestHeaderFooterAndContent(t *testing.T) {
	a := newTestApp()
	a.commands["p"] = model.CommandRecord{ID: "p", Status: model.StatusPendingApproval, Risk: model.RiskHigh}
	a.commands["r"] = model.CommandRecord{ID: "r", Status: model.StatusRunning, Risk: model.RiskLow}
	a.commands["f"] = model.CommandRecord{ID: "f", Status: model.StatusFailed, Risk: model.RiskMedium}
	header := a.header()
	if !strings.Contains(header, "Review 1") || !strings.Contains(header, "Running 1") || !strings.Contains(header, "Attention 1") {
		t.Fatalf("header=%q", header)
	}
	if !strings.Contains(a.footer(), "approve") {
		t.Fatalf("footer=%q", a.footer())
	}
	a.help = true
	if !strings.Contains(a.content(), "Keyboard Help") {
		t.Fatalf("help content=%q", a.content())
	}
	a.help = false
	a.page = pageConnections
	a.connections = []model.ConnectionStatus{{Host: "h", Status: "CONNECTED", User: "root", Addr: "127.0.0.1:22"}}
	if !strings.Contains(a.content(), "Connections") {
		t.Fatalf("connections content=%q", a.content())
	}
	a.page = pageSettings
	if !strings.Contains(a.content(), "Settings") {
		t.Fatalf("settings content=%q", a.content())
	}
}

func TestUpdateKeyAndViewFlow(t *testing.T) {
	a := newTestApp()
	modelAfter, cmd := a.Update(tea.WindowSizeMsg{Width: 132, Height: 40})
	a = modelAfter.(app)
	if cmd != nil {
		t.Fatal("window update returned command")
	}
	if a.width != 132 || a.viewport.Height >= 40 || a.viewport.Height < 35 {
		t.Fatalf("size not applied: width=%d viewport=%d", a.width, a.viewport.Height)
	}

	modelAfter, _ = a.Update(daemonMsg(protocol.Message{Type: "snapshot", Mode: config.ModeSensitive, Commands: []model.CommandRecord{{ID: "cmd", Status: model.StatusPendingApproval, Risk: model.RiskHigh, Host: "prod", Command: "restart"}}}))
	a = modelAfter.(app)
	if len(a.commands) != 1 {
		t.Fatalf("commands=%#v", a.commands)
	}

	modelAfter, _ = a.Update(key("2"))
	a = modelAfter.(app)
	if a.page != pageActivity {
		t.Fatalf("page=%d", a.page)
	}
	modelAfter, _ = a.Update(key("/"))
	a = modelAfter.(app)
	if a.inputMode != inputFilter {
		t.Fatalf("input mode=%d", a.inputMode)
	}
	modelAfter, _ = a.Update(key("p"))
	a = modelAfter.(app)
	modelAfter, _ = a.Update(key("r"))
	a = modelAfter.(app)
	modelAfter, _ = a.Update(key("o"))
	a = modelAfter.(app)
	modelAfter, _ = a.Update(key("d"))
	a = modelAfter.(app)
	if a.filterDraft != "prod" || a.filter != "" {
		t.Fatalf("filter draft=%q applied=%q", a.filterDraft, a.filter)
	}
	modelAfter, _ = a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	a = modelAfter.(app)
	if a.inputMode != inputNone || a.filter != "prod" {
		t.Fatalf("filter input did not finish: mode=%d filter=%q", a.inputMode, a.filter)
	}
	view := a.View()
	if !strings.Contains(view, "ssh-use") || !strings.Contains(view, "restart") {
		t.Fatalf("view=%q", view)
	}
}

func TestHandleConfirmKey(t *testing.T) {
	a := newTestApp()
	a.confirm = &confirmation{action: "test", payload: protocol.Message{Type: "snapshot"}}
	if cmd := a.handleConfirmKey(key("esc")); cmd != nil || a.confirm != nil || !strings.Contains(a.message, "cancelled") {
		t.Fatalf("cancel confirm failed cmd=%v confirm=%#v msg=%q", cmd, a.confirm, a.message)
	}
	a.confirm = &confirmation{action: "test", payload: protocol.Message{Type: "snapshot"}}
	cmd := a.handleConfirmKey(key("enter"))
	if cmd == nil || a.confirm == nil || !a.confirm.sending {
		t.Fatalf("confirm did not create command")
	}
	if cmd := a.handleConfirmKey(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Fatal("ctrl+c should quit while confirmation is sending")
	}
	a.requestSeq = 1
	modelAfter, _ := a.Update(requestResult{requestID: 1, err: errors.New("try again")})
	a = modelAfter.(app)
	if a.confirm == nil || a.confirm.sending || !strings.Contains(a.message, "try again") {
		t.Fatalf("failed confirmation was not retained: confirm=%#v msg=%q", a.confirm, a.message)
	}
	if cmd := a.handleConfirmKey(key("enter")); cmd == nil {
		t.Fatal("confirmation retry did not create command")
	}
	modelAfter, _ = a.Update(requestResult{requestID: a.requestSeq, message: protocol.Message{Type: "ack", OK: true}})
	a = modelAfter.(app)
	if a.confirm != nil {
		t.Fatalf("successful confirmation was not closed: %#v", a.confirm)
	}
}

func TestApprovalConfirmationRisksAndDirectReject(t *testing.T) {
	a := newApp(false, &subscription{}, []string{"medium"})
	a.commands["medium"] = model.CommandRecord{ID: "medium", Status: model.StatusPendingApproval, Risk: model.RiskMedium}
	if cmd := a.decide("approve"); cmd != nil || a.confirm == nil {
		t.Fatalf("configured risk should require confirmation: cmd=%v confirm=%#v", cmd, a.confirm)
	}

	a = newApp(false, &subscription{}, []string{"medium"})
	a.commands["high"] = model.CommandRecord{ID: "high", Status: model.StatusPendingApproval, Risk: model.RiskHigh}
	if cmd := a.decide("approve"); cmd == nil || a.confirm != nil {
		t.Fatalf("unconfigured risk should approve directly: cmd=%v confirm=%#v", cmd, a.confirm)
	}
	a.busy = false
	if cmd := a.decide("reject"); cmd == nil || a.confirm != nil {
		t.Fatalf("reject should not require confirmation: cmd=%v confirm=%#v", cmd, a.confirm)
	}
}

func TestActionMethodsWithoutSelection(t *testing.T) {
	a := newTestApp()
	if cmd := a.decide("approve"); cmd != nil || !strings.Contains(a.message, "no command") {
		t.Fatalf("decide cmd=%v msg=%q", cmd, a.message)
	}
	if cmd := a.cancelSelected(); cmd != nil || !strings.Contains(a.message, "no command") {
		t.Fatalf("cancel cmd=%v msg=%q", cmd, a.message)
	}
	a.page = pageConnections
	if cmd := a.closeSelectedConnection(); cmd != nil || !strings.Contains(a.message, "select") {
		t.Fatalf("close cmd=%v msg=%q", cmd, a.message)
	}
}

func TestConnectionActionsAndStyles(t *testing.T) {
	a := newTestApp()
	a.page = pageConnections
	a.connections = []model.ConnectionStatus{{Host: "h", OpenSessions: 1}}
	if cmd := a.closeSelectedConnection(); cmd != nil || !strings.Contains(a.message, "active sessions") {
		t.Fatalf("close active cmd=%v msg=%q", cmd, a.message)
	}
	a.connections[0].OpenSessions = 0
	if cmd := a.closeSelectedConnection(); cmd == nil {
		t.Fatal("expected close command")
	}
	a.busy = false
	if cmd := a.closeIdleConnections(); cmd == nil {
		t.Fatal("expected close idle command")
	}
	a.busy = false
	if cmd := a.togglePause(); cmd == nil {
		t.Fatal("expected pause command")
	}
	a.busy = false
	if cmd := a.emergencyStop(); cmd == nil {
		t.Fatal("expected emergency command")
	}
	a.busy = false
	if cmd := a.cycleMode(); cmd == nil {
		t.Fatal("expected mode command")
	}
	for _, risk := range []model.Risk{model.RiskCritical, model.RiskHigh, model.RiskMedium, model.RiskLow, model.Risk("x")} {
		_ = a.styles.risk(risk).Render("x")
	}
	for _, status := range []string{"PENDING", "RUNNING", "DONE", "FAILED", "REJECTED", "TIMEOUT", "QUEUED", "x"} {
		_ = a.styles.status(status).Render("x")
	}
	for _, mode := range []string{config.ModeAuto, config.ModeApproval, config.ModeSensitive} {
		_ = a.styles.mode(mode).Render("x")
	}
}

func TestSelectionFollowsCommandIdentity(t *testing.T) {
	a := newTestApp()
	now := time.Now()
	a.commands["medium"] = model.CommandRecord{ID: "medium", CreatedAt: now, Status: model.StatusPendingApproval, Risk: model.RiskMedium}
	a.restoreSelection()
	if a.selectedID != "medium" {
		t.Fatalf("selected ID = %q", a.selectedID)
	}
	a.commands["critical"] = model.CommandRecord{ID: "critical", CreatedAt: now.Add(time.Second), Status: model.StatusPendingApproval, Risk: model.RiskCritical}
	a.restoreSelection()
	rec, ok := a.selectedCommand()
	if !ok || rec.ID != "medium" || a.selected != 1 {
		t.Fatalf("selection changed target: rec=%#v index=%d", rec, a.selected)
	}
}

func TestActionsArePageScoped(t *testing.T) {
	a := newTestApp()
	a.commands["cmd"] = model.CommandRecord{ID: "cmd", Status: model.StatusPendingApproval, Risk: model.RiskLow}
	a.page = pageConnections
	a.connections = []model.ConnectionStatus{{Host: "host"}}
	if cmd := a.handleKey(key("a")); cmd != nil || a.busy {
		t.Fatal("approve must be disabled outside Review")
	}
	if cmd := a.handleKey(key("x")); cmd != nil || a.confirm != nil {
		t.Fatal("cancel must not target invisible command on Connections")
	}
}

func TestUnicodeFilterEditingAndCancel(t *testing.T) {
	a := newTestApp()
	a.filter = "old"
	a.filterDraft = "主机"
	a.inputMode = inputFilter
	a.handleFilterKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if a.filterDraft != "主" {
		t.Fatalf("backspace left invalid filter %q", a.filterDraft)
	}
	a.handleFilterKey(tea.KeyMsg{Type: tea.KeyEsc})
	if a.filter != "old" {
		t.Fatalf("escape changed filter to %q", a.filter)
	}
}

func TestSafeTextAndResponsiveTables(t *testing.T) {
	if got := safeText("ok\rspoof\x1b]52;c;secret\x07\x1b[2Jdone2J"); got != "okspoofdone2J" {
		t.Fatalf("safe text = %q", got)
	}
	for _, width := range []int{40, 60, 80, 120} {
		for _, widths := range [][]int{commandTableWidths(width), connectionTableWidths(width), policyTableWidths(width)} {
			total := len(widths) - 1
			for _, columnWidth := range widths {
				total += columnWidth
			}
			if total > width {
				t.Fatalf("columns total %d exceeds width %d: %#v", total, width, widths)
			}
		}
	}
}

func TestDetailAndConfirmationAreFocused(t *testing.T) {
	a := newTestApp()
	a.width = 80
	a.height = 24
	a.resizeViewport()
	a.commands["cmd"] = model.CommandRecord{ID: "cmd", Host: "prod", Command: "printf '\x1b[2J'", DisplayCommand: "restart service", Status: model.StatusPendingApproval, Risk: model.RiskHigh}
	a.restoreSelection()
	a.handleKey(key("enter"))
	if !a.detail || !strings.Contains(a.content(), "Command Detail") || strings.Contains(a.content(), "\x1b[2J") {
		t.Fatalf("detail state/content invalid: %q", a.content())
	}
	a.detail = false
	a.confirmApproval(a.commands["cmd"])
	content := a.content()
	if !strings.Contains(content, "APPROVE COMMAND?") || strings.Contains(content, "Pending Review") {
		t.Fatalf("confirmation is not focused: %q", content)
	}
}

func TestFilteredReviewEmptyStateIsHonest(t *testing.T) {
	a := newTestApp()
	a.commands["cmd"] = model.CommandRecord{ID: "cmd", Host: "prod", Status: model.StatusPendingApproval, Risk: model.RiskHigh}
	a.filter = "dev"
	view := a.reviewView()
	if !strings.Contains(view, "hidden by filter") || strings.Contains(view, "queue is clear") {
		t.Fatalf("filtered state = %q", view)
	}
}

func TestPendingTransitionFocusesReview(t *testing.T) {
	a := newTestApp()
	a.focusPending = true
	a.page = pageActivity
	a.filter = "hidden"
	a.filterDraft = "hidden"
	rec := model.CommandRecord{ID: "cmd", Status: model.StatusCreated, Risk: model.RiskHigh}
	a.apply(protocol.Message{Type: "command.created", Record: &rec})
	rec.Status = model.StatusPendingApproval
	a.apply(protocol.Message{Type: "command.pending", Record: &rec})
	if a.page != pageReview || a.selectedID != "cmd" || a.filter != "" || a.filterDraft != "" {
		t.Fatalf("pending transition did not focus visible review: page=%d selected=%q filter=%q", a.page, a.selectedID, a.filter)
	}
}

func TestConfigReloadEventsRefreshTUISettings(t *testing.T) {
	a := newTestApp()
	settings := &protocol.RuntimeSettings{
		Generation:          2,
		Mode:                config.ModeAuto,
		PolicyDefaultAction: "allow",
		BuiltinRules:        true,
		AuditStoreOutput:    "summary",
		AuditRetentionDays:  7,
		RedactSecrets:       true,
		ConfirmRisks:        []string{"low"},
		Theme:               "light",
		FocusPending:        false,
		BellOnPending:       true,
	}
	cmd := a.apply(protocol.Message{Type: "config.reloaded", Mode: config.ModeAuto, RuntimeSettings: settings})
	if cmd == nil || !a.refreshing || a.mode != config.ModeAuto || a.theme != "light" || a.focusPending || !a.bellPending {
		t.Fatalf("config reload not applied: cmd=%v mode=%s theme=%s focus=%v bell=%v", cmd, a.mode, a.theme, a.focusPending, a.bellPending)
	}
	if _, ok := a.confirmRisk[model.RiskLow]; !ok || len(a.confirmRisk) != 1 {
		t.Fatalf("confirm risks=%#v", a.confirmRisk)
	}
	recovered := *settings
	recovered.ConfirmRisks = []string{}
	recovered.Theme = "dark"
	recovered.Generation = 1
	a.applyRefreshSnapshot(protocol.Message{RuntimeSettings: &recovered})
	if len(a.confirmRisk) != 1 || a.theme != "light" {
		t.Fatalf("older snapshot rolled settings back: risks=%#v theme=%s", a.confirmRisk, a.theme)
	}
	recovered.Generation = 3
	a.applyRefreshSnapshot(protocol.Message{RuntimeSettings: &recovered})
	if len(a.confirmRisk) != 0 || a.theme != "dark" {
		t.Fatalf("newer snapshot settings not applied: risks=%#v theme=%s", a.confirmRisk, a.theme)
	}

	a.apply(protocol.Message{Type: "config.reload_failed", Error: "bad yaml"})
	if !strings.Contains(a.message, "bad yaml") {
		t.Fatalf("reload failure message=%q", a.message)
	}
}

func TestBusyRequestBlocksNewConfirmation(t *testing.T) {
	a := newTestApp()
	a.busy = true
	if cmd := a.handleKey(key("!")); cmd != nil || a.confirm != nil {
		t.Fatalf("busy request opened confirmation: cmd=%v confirm=%#v", cmd, a.confirm)
	}
	if !strings.Contains(a.message, "wait") {
		t.Fatalf("missing busy feedback: %q", a.message)
	}
}

func TestRevisionGapTriggersRefresh(t *testing.T) {
	a := newTestApp()
	a.lastRevision = 4
	cmd := a.apply(protocol.Message{Type: "daemon.paused", Revision: 6, Paused: true})
	if cmd == nil || a.streamHealthy || !a.paused || !strings.Contains(a.message, "gap") {
		t.Fatalf("gap recovery not started: cmd=%v healthy=%v paused=%v message=%q", cmd, a.streamHealthy, a.paused, a.message)
	}
}

func TestRefreshSnapshotPreservesNewerCommandState(t *testing.T) {
	a := newTestApp()
	a.streamHealthy = false
	a.streamAlive = false
	a.commands["cmd"] = model.CommandRecord{ID: "cmd", Status: model.StatusDone}
	a.applyRefreshSnapshot(protocol.Message{
		Type:        "snapshot",
		Commands:    []model.CommandRecord{{ID: "cmd", Status: model.StatusRunning}, {ID: "history", Status: model.StatusDone}},
		Connections: []model.ConnectionStatus{{Host: "host"}},
	})
	if a.commands["cmd"].Status != model.StatusDone || a.commands["history"].Status != model.StatusDone {
		t.Fatalf("refresh replaced newer state: %#v", a.commands)
	}
	if a.streamHealthy {
		t.Fatal("refresh snapshot marked disconnected stream live")
	}
	if len(a.connections) != 1 {
		t.Fatalf("connections not refreshed: %#v", a.connections)
	}
}

func TestViewportScrollSynchronizesContent(t *testing.T) {
	a := newTestApp()
	a.width = 80
	a.height = 12
	a.resizeViewport()
	a.help = true
	before := a.viewport.YOffset
	a.handleKey(key("pgdown"))
	if a.viewport.YOffset <= before {
		t.Fatalf("viewport did not scroll: before=%d after=%d", before, a.viewport.YOffset)
	}
}

func TestWaitDaemonCommands(t *testing.T) {
	msgCh := make(chan protocol.Message, 1)
	errCh := make(chan error, 1)
	a := newTestApp()
	a.sub = &subscription{ch: msgCh, err: errCh}
	msgCh <- protocol.Message{Type: "snapshot", OK: true}
	if got := a.waitDaemonMsg()(); got.(daemonMsg).Type != "snapshot" {
		t.Fatalf("msg=%#v", got)
	}
	errCh <- ioEOF{}
	if got := a.waitDaemonErr()(); got == nil {
		t.Fatal("expected daemon err")
	}
}

func TestRequestWithTimeout(t *testing.T) {
	orig := requestFn
	t.Cleanup(func() { requestFn = orig })
	requestFn = func(ctx context.Context, msg protocol.Message) (protocol.Message, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("request context has no deadline")
		}
		return protocol.Message{Type: "ack", OK: true, ID: msg.ID}, nil
	}
	got, err := requestWithTimeout(protocol.Message{Type: "x", ID: "cmd"})
	if err != nil || !got.OK || got.ID != "cmd" {
		t.Fatalf("response=%#v err=%v", got, err)
	}
	requestFn = func(context.Context, protocol.Message) (protocol.Message, error) {
		return protocol.Message{}, errors.New("bad")
	}
	if _, err := requestWithTimeout(protocol.Message{Type: "x"}); err == nil || err.Error() != "bad" {
		t.Fatalf("error=%v", err)
	}
}

func TestNewSubscription(t *testing.T) {
	orig := connectFn
	t.Cleanup(func() { connectFn = orig })
	connectFn = func() (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		go func() {
			dec := protocol.NewDecoder(serverConn)
			enc := protocol.NewEncoder(serverConn)
			var req protocol.Message
			_ = dec.Decode(&req)
			_ = enc.Encode(protocol.Message{Type: "snapshot", OK: true})
			_ = serverConn.Close()
		}()
		return clientConn, nil
	}
	sub, err := newSubscription()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.conn.Close()
	select {
	case msg := <-sub.ch:
		if msg.Type != "snapshot" {
			t.Fatalf("msg=%#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("no snapshot")
	}
	select {
	case <-sub.err:
	case <-time.After(time.Second):
		t.Fatal("no close error")
	}
}

func TestHandleKeyBranches(t *testing.T) {
	a := newTestApp()
	a.commands["cmd"] = model.CommandRecord{ID: "cmd", Status: model.StatusPendingApproval, Risk: model.RiskHigh, Host: "h", Command: "restart"}
	for _, k := range []string{"1", "2", "3", "4", "5", "j", "down", "k", "up", "?", "a", "esc", "r", "esc", "x", "esc", "!", "esc", "m", "esc"} {
		_ = a.handleKey(key(k))
		a.busy = false
	}
	a.page = pageConnections
	a.connections = []model.ConnectionStatus{{Host: "h"}}
	if cmd := a.handleKey(key("d")); cmd == nil {
		t.Fatal("d should return command")
	}
	a.busy = false
	if cmd := a.handleKey(key("D")); cmd != nil || a.confirm == nil {
		t.Fatal("D should open confirmation")
	}
	a.confirm = nil
	if cmd := a.handleKey(key("p")); cmd == nil {
		t.Fatal("p should return command")
	}
	if cmd := a.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Fatal("ctrl+c should quit")
	}
}

type ioEOF struct{}

func (ioEOF) Error() string { return "eof" }

func key(s string) tea.KeyMsg {
	switch s {
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	}
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func newTestApp() app {
	a := newApp(false, &subscription{}, config.Default().Approval.ConfirmRisks)
	a.width = 120
	a.height = 40
	return a
}
