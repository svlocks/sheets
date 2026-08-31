package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

type styles struct {
	title, subtle, accent, selected, danger, success   lipgloss.Style
	tab, activeTab, panel, activePanel, banner, status lipgloss.Style
	border                                             color.Color
}

func makeStyles(dark, noColor bool) styles {
	if noColor {
		plain := lipgloss.NoColor{}
		return styles{
			title:       lipgloss.NewStyle().Bold(true),
			subtle:      lipgloss.NewStyle(),
			accent:      lipgloss.NewStyle().Bold(true),
			selected:    lipgloss.NewStyle().Bold(true).Reverse(true),
			danger:      lipgloss.NewStyle().Bold(true),
			success:     lipgloss.NewStyle().Bold(true),
			tab:         lipgloss.NewStyle().Padding(0, 1),
			activeTab:   lipgloss.NewStyle().Bold(true).Underline(true).Padding(0, 1),
			panel:       lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(plain),
			activePanel: lipgloss.NewStyle().Border(lipgloss.ThickBorder()).BorderForeground(plain),
			banner:      lipgloss.NewStyle().Bold(true).Reverse(true),
			status:      lipgloss.NewStyle(),
			border:      plain,
		}
	}
	choose := lipgloss.LightDark(dark)
	fg := choose(lipgloss.Color("#17212B"), lipgloss.Color("#E7ECF2"))
	muted := choose(lipgloss.Color("#627083"), lipgloss.Color("#8B98A9"))
	accent := choose(lipgloss.Color("#6045B8"), lipgloss.Color("#B7A1FF"))
	selection := choose(lipgloss.Color("#E5DFFF"), lipgloss.Color("#352E52"))
	border := choose(lipgloss.Color("#B9C2CC"), lipgloss.Color("#45505E"))
	danger := choose(lipgloss.Color("#A7243B"), lipgloss.Color("#FF6B81"))
	success := choose(lipgloss.Color("#18794E"), lipgloss.Color("#69D89A"))
	warning := choose(lipgloss.Color("#7A4D00"), lipgloss.Color("#FFC857"))
	return styles{
		title:       lipgloss.NewStyle().Foreground(fg).Bold(true),
		subtle:      lipgloss.NewStyle().Foreground(muted),
		accent:      lipgloss.NewStyle().Foreground(accent).Bold(true),
		selected:    lipgloss.NewStyle().Foreground(fg).Background(selection).Bold(true),
		danger:      lipgloss.NewStyle().Foreground(danger).Bold(true),
		success:     lipgloss.NewStyle().Foreground(success).Bold(true),
		tab:         lipgloss.NewStyle().Foreground(muted).Padding(0, 1),
		activeTab:   lipgloss.NewStyle().Foreground(accent).Bold(true).Underline(true).Padding(0, 1),
		panel:       lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border),
		activePanel: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent),
		banner:      lipgloss.NewStyle().Foreground(warning).Bold(true),
		status:      lipgloss.NewStyle().Foreground(muted),
		border:      border,
	}
}
