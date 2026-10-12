package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/talmage89/dv/internal/pool"
	"github.com/talmage89/dv/keymap"
)

type pruneState int

const (
	pruneFetching pruneState = iota
	pruneListing
	pruneReady
	pruneRunning
	pruneDone
)

type pruneItem struct {
	c        pool.PruneCandidate
	selected bool
	result   string // "" until pruned; "ok" or an error message
}

// pruneScreen deletes local branches whose upstream was deleted on origin,
// freeing the worktrees that hold them.
type pruneScreen struct {
	state  pruneState
	items  []pruneItem
	cursor int
	err    string
}

func newPrune() *pruneScreen { return &pruneScreen{} }

func (s *pruneScreen) animating() bool { return s.state != pruneReady && s.state != pruneDone }

type pruneListMsg struct {
	cands []pool.PruneCandidate
	err   error
}

type pruneFetchMsg struct{ err error }

type prunedMsg struct {
	i   int
	err error
}

func (s *pruneScreen) init(a *App) tea.Cmd {
	if a.fetching {
		return nil // the background fetch's result is forwarded here
	}
	repo, ctx := a.repo, a.ctx
	return func() tea.Msg { return pruneFetchMsg{err: repo.Fetch(ctx)} }
}

func (s *pruneScreen) list(a *App) tea.Cmd {
	s.state = pruneListing
	repo, ctx := a.repo, a.ctx
	return func() tea.Msg {
		c, err := repo.PruneCandidates(ctx)
		return pruneListMsg{cands: c, err: err}
	}
}

func (s *pruneScreen) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case fetchDoneMsg:
		if s.state == pruneFetching {
			return s.update(a, pruneFetchMsg(msg))
		}
	case pruneFetchMsg:
		if msg.err != nil {
			// Prune from the last successful fetch rather than not at all.
			s.err = msg.err.Error() + " — showing results from the last fetch"
		}
		return tea.Batch(s.list(a), a.reloadSnapshot())
	case pruneListMsg:
		if msg.err != nil {
			s.err = msg.err.Error()
			s.state = pruneDone
			return nil
		}
		s.items = nil
		for _, c := range msg.cands {
			s.items = append(s.items, pruneItem{c: c, selected: c.DefaultSelected()})
		}
		s.state = pruneReady
		return nil
	case prunedMsg:
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

func (s *pruneScreen) key(a *App, msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if keys.Is(k, keymap.ListHelp) {
		a.openHelp()
		return nil
	}
	if s.state == pruneRunning {
		return nil
	}
	if keys.Is(k, keymap.ListBack) || (s.state == pruneDone && keys.Is(k, keymap.ListConfirm)) {
		a.pop()
		return a.refreshStatuses(nil)
	}
	if s.state != pruneReady || len(s.items) == 0 {
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

func (s *pruneScreen) start(a *App) tea.Cmd {
	n, dirty := 0, 0
	for _, it := range s.items {
		if it.selected {
			n++
			if it.c.Status.Dirty {
				dirty++
			}
		}
	}
	if n == 0 {
		a.notify(levelWarn, "Nothing selected")
		return nil
	}
	run := func() tea.Cmd {
		s.state = pruneRunning
		a.busy = "pruning"
		return s.next(a, 0)
	}
	if dirty > 0 {
		a.confirm("discard changes", []string{
			fmt.Sprintf("%s selected for pruning %s uncommitted changes. They will be discarded (reset --hard and clean -fd).",
				plural(dirty, "worktree", "worktrees"), map[bool]string{true: "has", false: "have"}[dirty == 1]),
		}, true, "discard and prune", run)
		return nil
	}
	return run()
}

// next prunes the next selected item at or after i, one at a time.
func (s *pruneScreen) next(a *App, i int) tea.Cmd {
	for ; i < len(s.items); i++ {
		if s.items[i].selected && s.items[i].result == "" {
			it, repo, ctx, idx := s.items[i], a.repo, a.ctx, i
			return func() tea.Msg {
				return prunedMsg{i: idx, err: repo.Prune(ctx, it.c, it.c.Status.Dirty)}
			}
		}
	}
	s.state = pruneDone
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
		a.notify(levelError, fmt.Sprintf("Pruned %d, %d failed", ok, failed))
	} else {
		a.notify(levelInfo, "Pruned "+plural(ok, "branch", "branches"))
	}
	return tea.Batch(a.reloadSnapshot(), a.loadLive())
}

func (s *pruneScreen) view(a *App, w, h int) string {
	inner := w - 4
	var body []string
	if s.err != "" {
		body = append(body, wrap(stWarn.Render(s.err), inner)...)
		body = append(body, "")
	}
	switch s.state {
	case pruneFetching:
		body = append(body, a.spinner.View()+" "+stMuted.Render("Fetching origin to see which branches were deleted…"))
		return panel("", body, w, h, true)
	case pruneListing:
		body = append(body, a.spinner.View()+" "+stMuted.Render("Checking branches…"))
		return panel("", body, w, h, true)
	}
	if len(s.items) == 0 {
		body = append(body, "Nothing to prune.",
			stMuted.Render("No local branch has had its upstream deleted on origin. Branches that were never pushed are not considered."))
		return panel("", body, w, h, true)
	}
	body = append(body, stMuted.Render("Branches whose upstream was deleted on origin, usually after a merge."), "")
	listH := h - 2 - len(body)
	start := 0
	if s.cursor >= listH {
		start = s.cursor - listH + 1
	}
	for i := start; i < len(s.items) && i-start < listH; i++ {
		body = append(body, s.renderItem(a, s.items[i], i == s.cursor && s.state == pruneReady, inner))
	}
	return panel("", body, w, h, true)
}

func (s *pruneScreen) renderItem(a *App, it pruneItem, selected bool, w int) string {
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
	case s.state == pruneRunning && it.selected:
		box = a.spinner.View()
	case !it.c.Selectable():
		box = stMuted.Render("–")
	case it.selected && it.c.Status.Dirty:
		box = stErr.Render("●")
	case it.selected:
		box = "●"
	default:
		box = stMuted.Render("○")
	}
	name := it.c.Branch
	var notes []string
	switch {
	case it.result != "" && it.result != "ok":
		notes = append(notes, stErr.Render(it.result))
	case it.c.InMain:
		notes = append(notes, stMuted.Render("checked out in the main worktree; switch it there first"))
	case it.c.Status.Err != "":
		notes = append(notes, stWarn.Render("status failed: "+firstLine(it.c.Status.Err)))
	default:
		if it.c.Worktree != "" {
			if it.c.Status.Dirty {
				t := plural(it.c.Status.Files, "uncommitted change", "uncommitted changes")
				if it.selected {
					t += " will be discarded"
				}
				notes = append(notes, stErr.Render(t))
			} else {
				notes = append(notes, stMuted.Render("frees a worktree"))
			}
			if it.c.Live > 0 {
				notes = append(notes, stMuted.Render("Claude running"))
			}
		} else {
			notes = append(notes, stMuted.Render("not checked out"))
		}
		if it.c.Parked {
			notes = append(notes, stMuted.Render("parked"))
		}
	}
	nameW := min(max(w/2, 20), w-10)
	label := pad(name, nameW)
	if selected {
		label = rainbow(label)
	}
	return bar + box + " " + label + "  " + truncate(strings.Join(notes, stMuted.Render(" · ")), w-nameW-6)
}

func (s *pruneScreen) status(a *App) string {
	if s.state < pruneReady || len(s.items) == 0 {
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

func (s *pruneScreen) hints(a *App) []hint {
	switch s.state {
	case pruneReady:
		if len(s.items) == 0 {
			return []hint{{keys.Key(keymap.ListBack), "back"}}
		}
		return []hint{{keys.Key(keymap.ListToggle), "toggle"}, {keys.Key(keymap.ListToggleAll), "all"}, {keys.Key(keymap.ListConfirm), "prune selected"}, {keys.Key(keymap.ListBack), "back"}}
	case pruneDone:
		return []hint{{keys.Key(keymap.ListConfirm) + "/" + keys.Key(keymap.ListBack), "back"}}
	}
	return []hint{{keys.Key(keymap.ListBack), "back"}}
}

func (s *pruneScreen) help() []helpSection {
	return []helpSection{{"Prune", []hint{
		{keys.Keys(keymap.ListToggle), "select or deselect a branch"},
		{keys.Keys(keymap.ListToggleAll), "select or deselect all"},
		{keys.Keys(keymap.ListConfirm), "prune the selection: free its worktree and delete the local branch"},
		{keys.Keys(keymap.ListBack), "back"},
	}}, {"Defaults", []hint{
		{"selected", "clean branches"},
		{"not selected", "branches with uncommitted changes (selecting discards them) and parked branches"},
	}}}
}

func (s *pruneScreen) title() string { return "prune" }
