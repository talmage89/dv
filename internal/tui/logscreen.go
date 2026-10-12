package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/talmage89/dv/internal/pool"
	"github.com/talmage89/dv/keymap"
)

// logScreen shows sync issues and everything dv did or failed to do this
// session.
type logScreen struct {
	scroll int
}

func newLogScreen() *logScreen { return &logScreen{} }

func (s *logScreen) update(a *App, msg tea.Msg) tea.Cmd {
	km, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	k := km.String()
	switch {
	case keys.Is(k, keymap.ListHelp):
		a.openHelp()
	case keys.Is(k, keymap.ListBack):
		a.pop()
	case keys.Is(k, keymap.ListUp):
		s.scroll = max(s.scroll-1, 0)
	case keys.Is(k, keymap.ListDown):
		s.scroll++
	case keys.Is(k, keymap.ListPageUp):
		s.scroll = max(s.scroll-10, 0)
	case keys.Is(k, keymap.ListPageDown):
		s.scroll += 10
	case keys.Is(k, keymap.LogSync):
		return a.syncCmd(pool.SyncAuto)
	case keys.Is(k, keymap.LogForceSync):
		a.confirm("force sync", []string{
			"Overwrites every .env that differs from the shared source (local edits are lost) and replaces regular files in .claude with links to the main worktree's copies.",
		}, true, "force sync", func() tea.Cmd { return a.syncCmd(pool.SyncForce) })
	case keys.Is(k, keymap.LogClear):
		a.log = nil
		s.scroll = 0
	}
	return nil
}

func (s *logScreen) view(a *App, w, h int) string {
	inner := w - 4
	const timeW = 10
	indent := strings.Repeat(" ", timeW)
	var lines []string
	heading := func(text string) { lines = append(lines, indent+stMuted.Render(text)) }
	entry := func(prefix, text string, st lipgloss.Style) {
		for k, l := range wrap(text, inner-timeW) {
			if k == 0 {
				lines = append(lines, pad(prefix, timeW)+st.Render(l))
			} else {
				lines = append(lines, indent+st.Render(l))
			}
		}
	}

	heading("sync")
	issues := a.sortedIssues()
	if len(issues) == 0 {
		entry("", "No issues.", stMuted)
	}
	for _, is := range issues {
		st := stWarn
		if is.Severity == pool.SeverityError {
			st = stErr
		}
		entry("", is.String(), st)
	}
	if len(issues) > 0 {
		entry("", "s syncs again; f forces it, overwriting diverged files.", stMuted)
	}
	if a.fetchErr != "" {
		lines = append(lines, "")
		heading("fetch")
		entry("", a.fetchErr, stWarn)
	}
	lines = append(lines, "")
	heading("activity")
	if len(a.log) == 0 {
		entry("", "Nothing yet.", stMuted)
	}
	for i := len(a.log) - 1; i >= 0; i-- {
		e := a.log[i]
		st := plain
		switch e.level {
		case levelWarn:
			st = stWarn
		case levelError:
			st = stErr
		}
		entry(stMuted.Render(e.at.Format("15:04:05")), e.text, st)
	}
	viewH := h - 1
	s.scroll = min(s.scroll, max(len(lines)-viewH, 0))
	return panel("", lines[s.scroll:], w, h, true)
}

func (s *logScreen) hints(a *App) []hint {
	return []hint{{keys.Pair(keymap.ListUp, keymap.ListDown), "scroll"}, {keys.Key(keymap.LogSync), "sync"}, {keys.Key(keymap.LogForceSync), "force sync"}, {keys.Key(keymap.LogClear), "clear"}, {keys.Key(keymap.ListBack), "back"}}
}

func (s *logScreen) help() []helpSection {
	return []helpSection{{"Log", []hint{
		{keys.Keys(keymap.ListUp) + " " + keys.Keys(keymap.ListDown) + " " + keys.Keys(keymap.ListPageUp) + " " + keys.Keys(keymap.ListPageDown), "scroll"},
		{keys.Keys(keymap.LogSync), "sync .env and .claude now (safe changes only)"},
		{keys.Keys(keymap.LogForceSync), "force sync: overwrite diverged files"},
		{keys.Keys(keymap.LogClear), "clear the activity log"},
		{keys.Keys(keymap.ListBack), "back"},
	}}}
}

func (s *logScreen) title() string { return "log" }
