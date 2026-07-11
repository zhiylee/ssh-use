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
	danger    lipgloss.Style
}

func newStyles() styles {
	return styles{
		header:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("24")).Padding(0, 1),
		footer:    lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Background(lipgloss.Color("235")).Padding(0, 1),
		tab:       lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Padding(0, 1),
		tabActive: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("62")).Padding(0, 1),
		title:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81")),
		section:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("110")),
		panel:     lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(1, 2),
		dialog:    lipgloss.NewStyle().Border(lipgloss.ThickBorder()).BorderForeground(lipgloss.Color("214")).Padding(1, 2).MarginTop(1),
		muted:     lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
		selected:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("62")),
		ok:        lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42")),
		danger:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("124")),
	}
}

func (s styles) risk(risk model.Risk) lipgloss.Style {
	switch risk {
	case model.RiskCritical:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("124"))
	case model.RiskHigh:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	case model.RiskMedium:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	case model.RiskLow:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	}
}

func (s styles) status(status string) lipgloss.Style {
	switch strings.ToUpper(status) {
	case "PENDING_APPROVAL", "PENDING":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	case "RUNNING":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("45"))
	case "DONE", "CONNECTED":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	case "FAILED", "BLOCKED", "CANCEL_FAILED", "ERROR":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	case "REJECTED", "CANCELLED", "CLOSED":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	case "TIMEOUT":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("201"))
	case "QUEUED", "PAUSED":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	default:
		return lipgloss.NewStyle()
	}
}

func (s styles) mode(mode string) lipgloss.Style {
	switch mode {
	case config.ModeAuto:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	case config.ModeApproval:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	default:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	}
}
