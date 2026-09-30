package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"agent-ssh/internal/config"
	"agent-ssh/internal/model"
)

type styles struct {
	header    lipgloss.Style
	footer    lipgloss.Style
	tab       lipgloss.Style
	tabActive lipgloss.Style
	title     lipgloss.Style
	section   lipgloss.Style
	panel     lipgloss.Style
	dialog    lipgloss.Style
	muted     lipgloss.Style
	selected  lipgloss.Style
	ok        lipgloss.Style
	warn      lipgloss.Style
	danger    lipgloss.Style
	mono      bool
}

func newStylesFor(theme string, safe bool) styles {
	if safe || strings.EqualFold(theme, "mono") {
		return styles{
			header:    lipgloss.NewStyle().Bold(true).Padding(0, 1),
			footer:    lipgloss.NewStyle().Padding(0, 1),
			tab:       lipgloss.NewStyle().Padding(0, 1),
			tabActive: lipgloss.NewStyle().Bold(true).Underline(true).Padding(0, 1),
			title:     lipgloss.NewStyle().Bold(true),
			section:   lipgloss.NewStyle().Bold(true),
			panel:     lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Padding(1, 2),
			dialog:    lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(1, 2),
			muted:     lipgloss.NewStyle().Faint(true),
			selected:  lipgloss.NewStyle().Bold(true).Reverse(true),
			ok:        lipgloss.NewStyle().Bold(true),
			warn:      lipgloss.NewStyle().Bold(true),
			danger:    lipgloss.NewStyle().Bold(true).Reverse(true),
			mono:      true,
		}
	}

	light := strings.EqualFold(theme, "light")
	background, footerBackground, headerBackground := "235", "254", "24"
	foreground, muted, border := "15", "241", "245"
	if light {
		background, footerBackground, headerBackground = "255", "252", "25"
		foreground, muted, border = "0", "242", "250"
	}
	return styles{
		header:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color(headerBackground)).Padding(0, 1),
		footer:    lipgloss.NewStyle().Foreground(lipgloss.Color(muted)).Background(lipgloss.Color(footerBackground)).Padding(0, 1),
		tab:       lipgloss.NewStyle().Foreground(lipgloss.Color(muted)).Padding(0, 1),
		tabActive: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("62")).Padding(0, 1),
		title:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("32")),
		section:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("67")),
		panel:     lipgloss.NewStyle().Foreground(lipgloss.Color(foreground)).Background(lipgloss.Color(background)).Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(border)).Padding(1, 2),
		dialog:    lipgloss.NewStyle().Foreground(lipgloss.Color(foreground)).Background(lipgloss.Color(background)).Border(lipgloss.DoubleBorder()).BorderForeground(lipgloss.Color("214")).Padding(1, 2),
		muted:     lipgloss.NewStyle().Foreground(lipgloss.Color(muted)),
		selected:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("62")),
		ok:        lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("35")),
		warn:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("172")),
		danger:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("124")),
	}
}

func (s styles) risk(risk model.Risk) lipgloss.Style {
	if s.mono {
		switch risk {
		case model.RiskCritical:
			return s.danger
		case model.RiskHigh, model.RiskMedium:
			return s.warn
		case model.RiskLow:
			return s.ok
		default:
			return s.muted
		}
	}
	switch risk {
	case model.RiskCritical:
		return s.danger
	case model.RiskHigh:
		return s.warn
	case model.RiskMedium:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("178"))
	case model.RiskLow:
		return s.ok
	default:
		return s.muted
	}
}

func (s styles) status(status string) lipgloss.Style {
	if s.mono {
		switch strings.ToUpper(status) {
		case "FAILED", "BLOCKED", "CANCEL_FAILED", "ERROR", "STALE":
			return s.danger
		case "PENDING_APPROVAL", "PENDING", "RUNNING", "TIMEOUT":
			return s.warn
		case "DONE", "CONNECTED":
			return s.ok
		default:
			return s.muted
		}
	}
	switch strings.ToUpper(status) {
	case "PENDING_APPROVAL", "PENDING":
		return s.warn
	case "RUNNING":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	case "DONE", "CONNECTED":
		return s.ok
	case "FAILED", "BLOCKED", "CANCEL_FAILED", "ERROR", "STALE":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	case "REJECTED", "CANCELLED", "CLOSED":
		return s.muted
	case "TIMEOUT":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("127"))
	case "QUEUED", "PAUSED":
		return s.muted
	default:
		return lipgloss.NewStyle()
	}
}

func (s styles) mode(mode string) lipgloss.Style {
	switch mode {
	case config.ModeAuto:
		return s.ok
	case config.ModeApproval:
		return s.danger
	default:
		return s.warn
	}
}
