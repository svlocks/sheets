package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/svlocks/sheets/internal/domain"
)

type revisionItem struct {
	info        domain.RevisionInfo
	title       string
	description string
	filter      string
}

func (i revisionItem) Title() string       { return i.title }
func (i revisionItem) Description() string { return i.description }
func (i revisionItem) FilterValue() string { return i.filter }

type timelineModel struct {
	list      list.Model
	revisions []domain.RevisionInfo
	live      domain.Revision
	width     int
	height    int
	dark      bool
	styles    styleSet
	loaded    bool
}

func newTimelineModel(styles styleSet, dark bool) timelineModel {
	delegate := list.NewDefaultDelegate()
	delegate.SetSpacing(0)
	listStyles, itemStyles := styles.listStyles(dark)
	delegate.Styles = itemStyles
	model := list.New(nil, delegate, 60, 20)
	model.Title = "Revision timeline"
	model.Styles = listStyles
	model.DisableQuitKeybindings()
	model.SetShowTitle(false)
	model.SetShowHelp(false)
	model.SetShowPagination(true)
	model.SetStatusBarItemName("revision", "revisions")
	return timelineModel{list: model, width: 60, height: 20, dark: dark, styles: styles}
}

func (m *timelineModel) setStyle(styles styleSet, dark bool) {
	m.styles = styles
	m.dark = dark
	listStyles, itemStyles := styles.listStyles(dark)
	m.list.Styles = listStyles
	delegate := list.NewDefaultDelegate()
	delegate.SetSpacing(0)
	delegate.Styles = itemStyles
	m.list.SetDelegate(delegate)
}

func (m *timelineModel) setSize(width, height int) {
	m.width = clampSize(width, 1)
	m.height = clampSize(height, 1)
	m.list.SetSize(m.width, m.height)
}

func (m *timelineModel) setRevisions(revisions []domain.RevisionInfo, live domain.Revision) tea.Cmd {
	selected, hadSelection := m.selectedRevision()
	m.revisions = append([]domain.RevisionInfo(nil), revisions...)
	m.live = live
	m.loaded = true
	items := make([]list.Item, 0, len(revisions)+1)
	for index := len(revisions) - 1; index >= 0; index-- {
		info := revisions[index]
		marker := ""
		if info.Revision == live {
			marker = " · LIVE"
		}
		actor := strings.TrimSpace(info.Actor)
		if actor == "" {
			actor = "unknown actor"
		}
		message := strings.TrimSpace(info.Message)
		if message == "" {
			message = "No revision message"
		}
		when := info.Time.Local().Format("2006-01-02 15:04:05 MST")
		items = append(items, revisionItem{
			info:        info,
			title:       fmt.Sprintf("Revision %d%s", info.Revision, marker),
			description: fmt.Sprintf("%s · %s · %s", when, actor, message),
			filter:      fmt.Sprintf("%d %s %s %s", info.Revision, when, actor, message),
		})
	}
	zero := domain.RevisionInfo{Revision: 0, Time: time.Time{}, Message: "Initial empty graph"}
	items = append(items, revisionItem{
		info: zero, title: "Revision 0 · INITIAL", description: "Empty graph before the first commit",
		filter: "0 initial empty graph",
	})
	cmd := m.list.SetItems(items)
	if hadSelection {
		m.selectRevision(selected)
	}
	return scopeListCmd(listTargetTimeline, 0, m.list.FilterValue(), cmd)
}

func (m *timelineModel) selectRevision(revision domain.Revision) bool {
	for index, item := range m.list.Items() {
		value, ok := item.(revisionItem)
		if ok && value.info.Revision == revision {
			m.list.Select(index)
			return true
		}
	}
	return false
}

func (m *timelineModel) update(msg tea.Msg) tea.Cmd {
	updated, cmd := m.list.Update(msg)
	m.list = updated
	return scopeListCmd(listTargetTimeline, 0, m.list.FilterValue(), cmd)
}

func (m *timelineModel) selectedRevision() (domain.Revision, bool) {
	item, ok := m.list.SelectedItem().(revisionItem)
	if !ok {
		return 0, false
	}
	return item.info.Revision, true
}

func (m *timelineModel) selectedInfo() (domain.RevisionInfo, bool) {
	item, ok := m.list.SelectedItem().(revisionItem)
	if !ok {
		return domain.RevisionInfo{}, false
	}
	return item.info, true
}

func (m *timelineModel) filtering() bool { return m.list.SettingFilter() }

func (m *timelineModel) wheel(delta int) {
	if delta < 0 {
		for range -delta {
			m.list.CursorUp()
		}
		return
	}
	for range delta {
		m.list.CursorDown()
	}
}

func (m timelineModel) view() string {
	if !m.loaded {
		return "Loading revision timeline…"
	}
	return m.list.View()
}
