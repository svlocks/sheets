package tui

import (
	"charm.land/bubbles/v2/key"
)

// Workspace identifies a primary, always-visible application destination.
type Workspace uint8

const (
	WorkWorkspace Workspace = iota
	RelationshipsWorkspace
	QueryWorkspace
	TimelineWorkspace
)

var workspaceNames = [...]string{"Work", "Relationships", "Query", "Timeline"}

func (w Workspace) String() string {
	if int(w) >= 0 && int(w) < len(workspaceNames) {
		return workspaceNames[w]
	}
	return "Unknown"
}

type keyMap struct {
	Quit          key.Binding
	Back          key.Binding
	Palette       key.Binding
	Help          key.Binding
	Refresh       key.Binding
	PreviousTab   key.Binding
	NextTab       key.Binding
	Work          key.Binding
	Relationships key.Binding
	Query         key.Binding
	Timeline      key.Binding
	TogglePane    key.Binding
	Find          key.Binding
	Open          key.Binding
	NewNode       key.Binding
	Edit          key.Binding
	Move          key.Binding
	Connect       key.Binding
	Delete        key.Binding
	ReturnLive    key.Binding
	RunQuery      key.Binding
	ExecQuery     key.Binding
	PreviousSet   key.Binding
	NextSet       key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Quit:          binding("ctrl+c", "quit", "ctrl+c"),
		Back:          binding("esc", "back", "esc"),
		Palette:       binding("ctrl+k", "commands", "ctrl+k"),
		Help:          binding("F10", "help", "f10"),
		Refresh:       binding("F5", "refresh", "f5"),
		PreviousTab:   binding("ctrl+pgup", "previous workspace", "ctrl+pgup"),
		NextTab:       binding("ctrl+pgdn", "next workspace", "ctrl+pgdown"),
		Work:          binding("F1", "work", "f1"),
		Relationships: binding("F2", "relationships", "f2"),
		Query:         binding("F3", "query", "f3"),
		Timeline:      binding("F4", "timeline", "f4"),
		TogglePane:    binding("tab", "switch pane", "tab", "shift+tab"),
		Find:          binding("/", "find", "/"),
		Open:          binding("enter", "open", "enter"),
		NewNode:       binding("n", "new node", "n"),
		Edit:          binding("e", "edit", "e"),
		Move:          binding("m", "move / order", "m"),
		Connect:       binding("c", "connect", "c"),
		Delete:        binding("d", "delete", "d"),
		ReturnLive:    binding("F6", "return live", "f6"),
		RunQuery:      binding("ctrl+r", "run read-only", "ctrl+r", "ctrl+enter"),
		ExecQuery:     binding("ctrl+x", "execute write-capable", "ctrl+x"),
		PreviousSet:   binding("[", "previous result", "["),
		NextSet:       binding("]", "next result", "]"),
	}
}

func binding(helpKey, helpDescription string, keys ...string) key.Binding {
	return key.NewBinding(key.WithKeys(keys...), key.WithHelp(helpKey, helpDescription))
}

func enabledBinding(value key.Binding, enabled bool) key.Binding {
	value.SetEnabled(enabled)
	return value
}
