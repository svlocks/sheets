package tui

import (
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
)

type listMessageTarget uint8

const (
	listTargetRelationships listMessageTarget = iota + 1
	listTargetTimeline
	listTargetPicker
)

// listFilterMatchesMsg keeps Bubbles' asynchronous filtering result attached
// to the component and filter value that produced it. FilterMatchesMsg itself
// has no component identity, so routing it by the currently visible workspace
// can corrupt a different list after fast workspace or overlay navigation.
type listFilterMatchesMsg struct {
	target listMessageTarget
	serial uint64
	filter string
	msg    list.FilterMatchesMsg
}

func scopeListCmd(target listMessageTarget, serial uint64, filter string, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		switch value := msg.(type) {
		case tea.BatchMsg:
			batch := make(tea.BatchMsg, 0, len(value))
			for _, nested := range value {
				if scoped := scopeListCmd(target, serial, filter, nested); scoped != nil {
					batch = append(batch, scoped)
				}
			}
			return batch
		case list.FilterMatchesMsg:
			return listFilterMatchesMsg{target: target, serial: serial, filter: filter, msg: value}
		default:
			return msg
		}
	}
}
