package tui

import (
	"fmt"
	"sort"
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
	filterSeq uint64
	width     int
	height    int
	dark      bool
	styles    styleSet
	loaded    bool
	loading   bool
	older     bool
	end       bool
	err       error
	restore   *domain.Revision
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
	m.list.SetSize(m.width, max(1, m.height-1))
}

func (m *timelineModel) setRevisions(revisions []domain.RevisionInfo, live domain.Revision, includeInitial bool) tea.Cmd {
	selected, hadSelection := m.selectedRevision()
	m.revisions = append([]domain.RevisionInfo(nil), revisions...)
	sort.Slice(m.revisions, func(i, j int) bool { return m.revisions[i].Revision > m.revisions[j].Revision })
	m.live = live
	m.loaded = true
	items := make([]list.Item, 0, len(m.revisions)+1)
	seen := make(map[domain.Revision]struct{}, len(m.revisions))
	for _, info := range m.revisions {
		if info.Revision == 0 {
			includeInitial = true
			continue
		}
		if _, exists := seen[info.Revision]; exists {
			continue
		}
		seen[info.Revision] = struct{}{}
		marker := ""
		if info.Revision == live {
			marker = " · LIVE"
		}
		actor := terminalLine(truncateRunes(strings.TrimSpace(info.Actor), maxTimelineTextRunes))
		if actor == "" {
			actor = "unknown actor"
		}
		message := terminalLine(truncateRunes(strings.TrimSpace(info.Message), maxTimelineTextRunes))
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
	if includeInitial {
		zero := domain.RevisionInfo{Revision: 0, Time: time.Time{}, Message: "Initial empty graph"}
		items = append(items, revisionItem{
			info: zero, title: "Revision 0 · INITIAL", description: "Empty graph before the first commit",
			filter: "0 initial empty graph",
		})
	}
	m.filterSeq++
	cmd := m.list.SetItems(items)
	if hadSelection {
		if !m.selectRevision(selected) {
			value := selected
			m.restore = &value
		}
	}
	return scopeListCmd(listTargetTimeline, 0, m.filterSeq, m.list.FilterValue(), cmd)
}

func (m *timelineModel) setPaging(loading, older, end bool, err error) {
	m.loading = loading
	m.older = older
	m.end = end
	m.err = err
}

func (m *timelineModel) selectRevision(revision domain.Revision) bool {
	for index, item := range m.list.VisibleItems() {
		value, ok := item.(revisionItem)
		if ok && value.info.Revision == revision {
			m.list.Select(index)
			m.restore = nil
			return true
		}
	}
	return false
}

func (m *timelineModel) update(msg tea.Msg) tea.Cmd {
	beforeFilter := m.list.FilterValue()
	updated, cmd := m.list.Update(msg)
	m.list = updated
	if beforeFilter != m.list.FilterValue() {
		m.filterSeq++
	}
	if m.restore != nil {
		m.selectRevision(*m.restore)
	}
	return scopeListCmd(listTargetTimeline, 0, m.filterSeq, m.list.FilterValue(), cmd)
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

func (m *timelineModel) nearEnd() bool {
	items := m.list.Items()
	return len(items) > 0 && !m.list.IsFiltered() && m.list.GlobalIndex() >= len(items)-3
}

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
		if m.err != nil {
			return "Revision timeline unavailable · F5 retries: " + terminalLine(m.err.Error())
		}
		return "Loading revision timeline…"
	}
	if m.height <= 1 {
		return m.list.View()
	}
	status := "More older revisions · press o to load"
	switch {
	case m.loading && m.older:
		status = "Loading older revisions…"
	case m.loading:
		status = "Refreshing revision timeline…"
	case m.err != nil && m.older:
		status = "Older revisions failed · press o to retry: " + terminalLine(m.err.Error())
	case m.err != nil:
		status = "Timeline refresh failed · press F5 to retry: " + terminalLine(m.err.Error())
	case m.end:
		status = "Beginning of revision history reached"
	}
	style := m.styles.subtle
	if m.err != nil {
		style = m.styles.error
	}
	return m.list.View() + "\n" + fitLine(style.Render(status), m.width)
}
