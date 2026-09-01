package tui

import (
	"image/color"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/tree"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type styleSet struct {
	appName       lipgloss.Style
	project       lipgloss.Style
	tab           lipgloss.Style
	activeTab     lipgloss.Style
	readOnly      lipgloss.Style
	paneTitle     lipgloss.Style
	focusedTitle  lipgloss.Style
	subtle        lipgloss.Style
	status        lipgloss.Style
	success       lipgloss.Style
	error         lipgloss.Style
	border        lipgloss.Style
	focusedBorder lipgloss.Style
	modalBorder   lipgloss.Style
	badge         lipgloss.Style
	code          lipgloss.Style
	accent        color.Color
	dim           color.Color
	warning       color.Color
	danger        color.Color
	noColor       bool
}

func makeStyles(dark, noColor bool) styleSet {
	if noColor {
		border := lipgloss.NewStyle().Border(lipgloss.ASCIIBorder())
		return styleSet{
			border:        border,
			focusedBorder: border,
			modalBorder:   border,
			noColor:       true,
		}
	}
	accent := lipgloss.Color("39")
	dim := lipgloss.Color("244")
	warning := lipgloss.Color("214")
	danger := lipgloss.Color("203")
	if !dark {
		accent = lipgloss.Color("27")
		dim = lipgloss.Color("242")
		warning = lipgloss.Color("130")
		danger = lipgloss.Color("160")
	}
	borderColor := lipgloss.Color("238")
	if !dark {
		borderColor = lipgloss.Color("252")
	}
	return styleSet{
		appName:       lipgloss.NewStyle().Bold(true).Foreground(accent),
		project:       lipgloss.NewStyle().Foreground(dim),
		tab:           lipgloss.NewStyle().Foreground(dim).Padding(0, 1),
		activeTab:     lipgloss.NewStyle().Bold(true).Foreground(accent).Padding(0, 1).Underline(true),
		readOnly:      lipgloss.NewStyle().Bold(true).Foreground(danger),
		paneTitle:     lipgloss.NewStyle().Foreground(dim),
		focusedTitle:  lipgloss.NewStyle().Bold(true).Foreground(accent),
		subtle:        lipgloss.NewStyle().Foreground(dim),
		status:        lipgloss.NewStyle().Foreground(dim),
		success:       lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
		error:         lipgloss.NewStyle().Foreground(danger),
		border:        lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(borderColor),
		focusedBorder: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent),
		modalBorder:   lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).BorderForeground(accent),
		badge:         lipgloss.NewStyle().Foreground(accent),
		code:          lipgloss.NewStyle().Foreground(lipgloss.Color("212")),
		accent:        accent,
		dim:           dim,
		warning:       warning,
		danger:        danger,
	}
}

func (s styleSet) treeStyles(dark bool) tree.Styles {
	if s.noColor {
		return tree.Styles{}
	}
	styles := tree.DefaultStyles(dark)
	styles.SelectedNodeStyle = lipgloss.NewStyle().Bold(true).Foreground(s.accent)
	styles.CursorStyle = lipgloss.NewStyle().Foreground(s.accent)
	styles.EnumeratorStyle = lipgloss.NewStyle().Foreground(s.dim)
	styles.IndenterStyle = lipgloss.NewStyle().Foreground(s.dim)
	styles.OpenIndicatorStyle = lipgloss.NewStyle().Foreground(s.dim)
	styles.RootNodeStyle = lipgloss.NewStyle().Bold(true)
	return styles
}

func (s styleSet) listStyles(dark bool) (list.Styles, list.DefaultItemStyles) {
	if s.noColor {
		styles := list.Styles{
			Filter:     plainTextInputStyles(),
			DividerDot: lipgloss.NewStyle().SetString(" · "),
		}
		items := list.DefaultItemStyles{
			NormalTitle:   lipgloss.NewStyle().PaddingLeft(2),
			NormalDesc:    lipgloss.NewStyle().PaddingLeft(2),
			SelectedTitle: lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).PaddingLeft(1),
			SelectedDesc:  lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).PaddingLeft(1),
			DimmedTitle:   lipgloss.NewStyle().PaddingLeft(2),
			DimmedDesc:    lipgloss.NewStyle().PaddingLeft(2),
		}
		return styles, items
	}
	styles := list.DefaultStyles(dark)
	styles.Title = styles.Title.Foreground(s.accent)
	styles.DefaultFilterCharacterMatch = styles.DefaultFilterCharacterMatch.Foreground(s.accent)
	items := list.NewDefaultItemStyles(dark)
	items.SelectedTitle = items.SelectedTitle.Foreground(s.accent).BorderForeground(s.accent)
	items.SelectedDesc = items.SelectedDesc.Foreground(s.dim).BorderForeground(s.accent)
	items.FilterMatch = items.FilterMatch.Foreground(s.accent)
	return styles, items
}

func (s styleSet) tableStyles() table.Styles {
	if s.noColor {
		return table.Styles{
			Header:   lipgloss.NewStyle().Padding(0, 1),
			Cell:     lipgloss.NewStyle().Padding(0, 1),
			Selected: lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.NormalBorder()),
		}
	}
	styles := table.DefaultStyles()
	styles.Header = styles.Header.Foreground(s.accent).BorderBottom(true).BorderForeground(s.dim)
	styles.Selected = styles.Selected.Foreground(s.accent)
	return styles
}

func (s styleSet) helpStyles(dark bool) help.Styles {
	if s.noColor {
		return help.Styles{}
	}
	styles := help.DefaultStyles(dark)
	styles.ShortKey = styles.ShortKey.Foreground(s.accent)
	styles.FullKey = styles.FullKey.Foreground(s.accent)
	return styles
}

func (s styleSet) textareaStyles(dark bool) textarea.Styles {
	if s.noColor {
		return plainTextareaStyles()
	}
	styles := textarea.DefaultStyles(dark)
	styles.Focused.Prompt = styles.Focused.Prompt.Foreground(s.accent)
	styles.Focused.LineNumber = styles.Focused.LineNumber.Foreground(s.dim)
	styles.Focused.CursorLineNumber = styles.Focused.CursorLineNumber.Foreground(s.accent)
	return styles
}

func plainTextareaStyles() textarea.Styles {
	return textarea.Styles{}
}

func plainTextInputStyles() textinput.Styles {
	return textinput.Styles{}
}

func fitLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = strings.ReplaceAll(value, "\n", " ")
	value = ansi.Truncate(value, width, "…")
	if gap := width - ansi.StringWidth(value); gap > 0 {
		value += strings.Repeat(" ", gap)
	}
	return value
}

func clampSize(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}
