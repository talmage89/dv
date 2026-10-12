package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/talmage89/dv/internal/pool"
	"github.com/talmage89/dv/keymap"
)

type sweepState int

const (
	sweepListing sweepState = iota
	sweepReady
	sweepRunning
	sweepDone
)

type sweepItem struct {
	c        pool.SweepCandidate
	selected bool
	result   string // "" until freed; "ok" or an error message
}

// sweepScreen frees worktrees holding branches that are not the user's,
// keeping the branches.
type sweepScreen struct {
	state  sweepState
	items  []sweepItem
	cursor int
	err    string
}

func newSweep() *sweepScreen { return &sweepScreen{} }

func (s *sweepScreen) animating() bool { return s.state != sweepReady && s.state != sweepDone }

type sweepListMsg struct {
	cands []pool.SweepCandidate
	err   error
}

type sweptMsg struct {
	i   int
	err error
}

func (s *sweepScreen) init(a *App) tea.Cmd {
	repo, ctx := a.repo, a.ctx
	return func() tea.Msg {
		c, err := repo.SweepCandidates(ctx)
		return sweepListMsg{cands: c, err: err}
	}
}

func (s *sweepScreen) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case sweepListMsg:
		if msg.err != nil {
			s.err = msg.err.Error()
			s.state = sweepDone
			return nil
		}
		s.items = nil
		for _, c := range msg.cands {
			s.items = append(s.items, sweepItem{c: c, selected: c.DefaultSelected()})
		}
		s.state = sweepReady
		return nil
	case sweptMsg:
		it := &s.items[msg.i]
		if msg.err != nil {
			it.result = friendly(msg.err).Error()
		} else {
			it.result = "ok"
		}
		return s.next(a, msg.i+1)
	case tea.KeyPressMsg:
		return s.key(a, msg)
	}
	return nil
}

func (s *sweepScreen) key(a *App, msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if keys.Is(k, keymap.ListHelp) {
		a.openHelp()
		return nil
	}
	if s.state == sweepRunning {
		return nil
	}
	if keys.Is(k, keymap.ListBack) || (s.state == sweepDone && keys.Is(k, keymap.ListConfirm)) {
		a.pop()
		return a.refreshStatuses(nil)
	}
	if s.state != sweepReady || len(s.items) == 0 {
		return nil
	}
	switch {
	case keys.Is(k, keymap.ListUp):
		s.cursor = max(s.cursor-1, 0)
	case keys.Is(k, keymap.ListDown):
		s.cursor = min(s.cursor+1, len(s.items)-1)
	case keys.Is(k, keymap.ListToggle):
		if it := &s.items[s.cursor]; it.c.Selectable() {
			it.selected = !it.selected
		}
	case keys.Is(k, keymap.ListToggleAll):
		all := true
		for _, it := range s.items {
			if it.c.Selectable() && !it.selected {
				all = false
			}
		}
		for i := range s.items {
			s.items[i].selected = !all && s.items[i].c.Selectable()
		}
	case keys.Is(k, keymap.ListConfirm):
		return s.start(a)
	}
	return nil
}

func (s *sweepScreen) start(a *App) tea.Cmd {
	n := 0
	for _, it := range s.items {
		if it.selected {
			n++
		}
	}
	if n == 0 {
		a.notify(levelWarn, "Nothing selected")
		return nil
	}
	s.state = sweepRunning
	a.busy = "sweeping"
	return s.next(a, 0)
}

// next frees the next selected worktree at or after i, one at a time.
func (s *sweepScreen) next(a *App, i int) tea.Cmd {
	for ; i < len(s.items); i++ {
		if s.items[i].selected && s.items[i].result == "" {
			branch, repo, ctx, idx := s.items[i].c.Branch, a.repo, a.ctx, i
			return func() tea.Msg {
				return sweptMsg{i: idx, err: repo.Release(ctx, branch)}
			}
		}
	}
	s.state = sweepDone
	a.busy = ""
	ok, failed := 0, 0
	for _, it := range s.items {
		switch {
		case it.result == "ok":
			ok++
		case it.result != "":
			failed++
		}
	}
	if failed > 0 {
		a.notify(levelError, fmt.Sprintf("Freed %d, %d failed", ok, failed))
	} else {
		a.notify(levelInfo, "Freed "+plural(ok, "worktree", "worktrees"))
	}
	return tea.Batch(a.reloadSnapshot(), a.loadLive())
}

func (s *sweepScreen) view(a *App, w, h int) string {
	inner := w - 4
	var body []string
	if s.err != "" {
		body = append(body, wrap(stWarn.Render(s.err), inner)...)
		body = append(body, "")
	}
	switch {
	case s.state == sweepListing:
		body = append(body, a.spinner.View()+" "+stMuted.Render("Checking whose branches are checked out…"))
		return panel("", body, w, h, true)
	case s.err != "":
		return panel("", body, w, h, true)
	case len(s.items) == 0:
		body = append(body, "Nothing to sweep.",
			stMuted.Render("Every checked-out branch has a commit by "+strings.Join(a.repo.Emails, " or ")+"."))
		return panel("", body, w, h, true)
	}
	body = append(body, stMuted.Render("Worktrees holding branches with no commit of yours. Freeing one keeps the branch."), "")
	listH := h - 2 - len(body)
	start := 0
	if s.cursor >= listH {
		start = s.cursor - listH + 1
	}
	for i := start; i < len(s.items) && i-start < listH; i++ {
		body = append(body, s.renderItem(a, s.items[i], i == s.cursor && s.state == sweepReady, inner))
	}
	return panel("", body, w, h, true)
}

func (s *sweepScreen) renderItem(a *App, it sweepItem, selected bool, w int) string {
	bar := "  "
	if selected {
		bar = selMarker() + " "
	}
	var box string
	switch {
	case it.result == "ok":
		box = stMuted.Render("✓")
	case it.result != "":
		box = stErr.Render("×")
	case s.state == sweepRunning && it.selected:
		box = a.spinner.View()
	case !it.c.Selectable():
		box = stMuted.Render("–")
	case it.selected:
		box = "●"
	default:
		box = stMuted.Render("○")
	}
	notes := []string{stMuted.Render(filepath.Base(it.c.Worktree))}
	switch {
	case it.result != "" && it.result != "ok":
		notes = append(notes, stErr.Render(it.result))
	case it.c.Status.Err != "":
		notes = append(notes, stWarn.Render("status failed: "+firstLine(it.c.Status.Err)))
	case it.c.Status.Dirty:
		notes = append(notes, stErr.Render(plural(it.c.Status.Files, "uncommitted change", "uncommitted changes")))
	}
	if it.c.Live > 0 {
		notes = append(notes, stWarn.Render("Claude running"))
	}
	nameW := min(max(w/2, 20), w-10)
	label := pad(it.c.Branch, nameW)
	if selected {
		label = rainbow(label)
	}
	return bar + box + " " + label + "  " + truncate(strings.Join(notes, stMuted.Render(" · ")), w-nameW-6)
}

func (s *sweepScreen) status(a *App) string {
	if s.state < sweepReady || len(s.items) == 0 {
		return ""
	}
	n := 0
	for _, it := range s.items {
		if it.selected {
			n++
		}
	}
	return fmt.Sprintf("%d of %d selected", n, len(s.items))
}

func (s *sweepScreen) hints(a *App) []hint {
	switch s.state {
	case sweepReady:
		if len(s.items) == 0 {
			return []hint{{keys.Key(keymap.ListBack), "back"}}
		}
		return []hint{{keys.Key(keymap.ListToggle), "toggle"}, {keys.Key(keymap.ListToggleAll), "all"}, {keys.Key(keymap.ListConfirm), "free selected"}, {keys.Key(keymap.ListBack), "back"}}
	case sweepDone:
		return []hint{{keys.Key(keymap.ListConfirm) + "/" + keys.Key(keymap.ListBack), "back"}}
	}
	return []hint{{keys.Key(keymap.ListBack), "back"}}
}

func (s *sweepScreen) help() []helpSection {
	return []helpSection{{"Sweep", []hint{
		{keys.Keys(keymap.ListToggle), "select or deselect a worktree"},
		{keys.Keys(keymap.ListToggleAll), "select or deselect all"},
		{keys.Keys(keymap.ListConfirm), "free the selection, keeping the branches"},
		{keys.Keys(keymap.ListBack), "back"},
	}}, {"Defaults", []hint{
		{"selected", "clean worktrees with no Claude session"},
		{"not selected", "worktrees with a Claude session running"},
		{"unavailable", "worktrees with uncommitted changes"},
	}}}
}

func (s *sweepScreen) title() string { return "sweep" }
