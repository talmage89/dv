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
	sweepFetching sweepState = iota
	sweepListing
	sweepReady
	sweepRunning
	sweepDone
)

type sweepItem struct {
	c        pool.SweepCandidate
	selected bool
	result   string // "" until swept; "ok" or an error message
}

// sweepScreen clears away branches that no longer need a place: it deletes
// branches whose upstream was deleted on origin, and frees worktrees holding
// branches that are not the user's, keeping those branches.
type sweepScreen struct {
	state  sweepState
	items  []sweepItem
	cursor int
	err    string
}

func newSweep() *sweepScreen { return &sweepScreen{} }

func (s *sweepScreen) animating() bool { return s.state != sweepReady && s.state != sweepDone }

type sweepFetchMsg struct{ err error }

type sweepListMsg struct {
	cands []pool.SweepCandidate
	err   error
}

type sweptMsg struct {
	i   int
	err error
}

func (s *sweepScreen) init(a *App) tea.Cmd {
	if a.fetching {
		return nil // the background fetch's result is forwarded here
	}
	repo, ctx := a.repo, a.ctx
	return func() tea.Msg { return sweepFetchMsg{err: repo.Fetch(ctx)} }
}

func (s *sweepScreen) list(a *App) tea.Cmd {
	s.state = sweepListing
	repo, ctx := a.repo, a.ctx
	return func() tea.Msg {
		c, err := repo.SweepCandidates(ctx)
		return sweepListMsg{cands: c, err: err}
	}
}

func (s *sweepScreen) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case fetchDoneMsg:
		if s.state == sweepFetching {
			return s.update(a, sweepFetchMsg(msg))
		}
	case sweepFetchMsg:
		if msg.err != nil {
			// Sweep from the last successful fetch rather than not at all.
			s.err = msg.err.Error() + " — showing results from the last fetch"
		}
		return tea.Batch(s.list(a), a.reloadSnapshot())
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
		s.state = sweepRunning
		a.busy = "sweeping"
		return s.next(a, 0)
	}
	if dirty > 0 {
		a.confirm("discard changes", []string{
			fmt.Sprintf("%s selected for deletion %s uncommitted changes. They will be discarded (reset --hard and clean -fd).",
				plural(dirty, "worktree", "worktrees"), map[bool]string{true: "has", false: "have"}[dirty == 1]),
		}, true, "discard and sweep", run)
		return nil
	}
	return run()
}

// next sweeps the next selected item at or after i, one at a time.
func (s *sweepScreen) next(a *App, i int) tea.Cmd {
	for ; i < len(s.items); i++ {
		if s.items[i].selected && s.items[i].result == "" {
			it, repo, ctx, idx := s.items[i], a.repo, a.ctx, i
			return func() tea.Msg {
				return sweptMsg{i: idx, err: repo.Sweep(ctx, it.c, it.c.Status.Dirty)}
			}
		}
	}
	s.state = sweepDone
	a.busy = ""
	deleted, freed, failed := 0, 0, 0
	for _, it := range s.items {
		switch {
		case it.result == "ok" && it.c.Gone:
			deleted++
		case it.result == "ok":
			freed++
		case it.result != "":
			failed++
		}
	}
	var parts []string
	if deleted > 0 {
		parts = append(parts, "deleted "+plural(deleted, "branch", "branches"))
	}
	if freed > 0 {
		parts = append(parts, "freed "+plural(freed, "worktree", "worktrees"))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	text := strings.Join(parts, ", ")
	text = strings.ToUpper(text[:1]) + text[1:]
	if failed > 0 {
		a.notify(levelError, text)
	} else {
		a.notify(levelInfo, text)
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
	case s.state == sweepFetching:
		body = append(body, a.spinner.View()+" "+stMuted.Render("Fetching origin to see which branches were deleted…"))
		return panel("", body, w, h, true)
	case s.state == sweepListing:
		body = append(body, a.spinner.View()+" "+stMuted.Render("Checking branches…"))
		return panel("", body, w, h, true)
	case len(s.items) == 0 && s.state == sweepDone:
		return panel("", body, w, h, true)
	case len(s.items) == 0:
		body = append(body, "Nothing to sweep.",
			stMuted.Render("No local branch was deleted on origin, and every checked-out branch has a commit by "+strings.Join(a.repo.Emails, " or ")+"."))
		return panel("", body, w, h, true)
	}
	var lines []string
	cursorLine := 0
	for i, it := range s.items {
		if i == 0 || it.c.Gone != s.items[i-1].c.Gone {
			if i > 0 {
				lines = append(lines, "")
			}
			if it.c.Gone {
				lines = append(lines, stMuted.Render("Deleted on origin, usually after a merge. Sweeping deletes the local branch."))
			} else {
				lines = append(lines, stMuted.Render("Branches with no commit of yours. Sweeping frees the worktree and keeps the branch."))
			}
		}
		if i == s.cursor {
			cursorLine = len(lines)
		}
		lines = append(lines, s.renderItem(a, it, i == s.cursor && s.state == sweepReady, inner))
	}
	listH := h - 2 - len(body)
	start := 0
	if cursorLine >= listH {
		start = cursorLine - listH + 1
	}
	body = append(body, lines[start:min(len(lines), start+listH)]...)
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
	case it.selected && it.c.Status.Dirty:
		box = stErr.Render("●")
	case it.selected:
		box = "●"
	default:
		box = stMuted.Render("○")
	}
	var notes []string
	switch {
	case it.result != "" && it.result != "ok":
		notes = append(notes, stErr.Render(it.result))
	case it.c.InMain:
		notes = append(notes, stMuted.Render("checked out in the main worktree; switch it there first"))
	case it.c.Worktree == "":
		notes = append(notes, stMuted.Render("not checked out"))
	case it.c.Status.Err != "":
		notes = append(notes, stMuted.Render(filepath.Base(it.c.Worktree)), stWarn.Render("status failed: "+firstLine(it.c.Status.Err)))
	default:
		notes = append(notes, stMuted.Render(filepath.Base(it.c.Worktree)))
		if it.c.Status.Dirty && it.result == "" {
			t := plural(it.c.Status.Files, "uncommitted change", "uncommitted changes")
			if it.selected {
				t += " will be discarded"
			}
			notes = append(notes, stErr.Render(t))
		}
	}
	if it.c.Live > 0 && it.result == "" {
		notes = append(notes, stWarn.Render("Claude running"))
	}
	if it.c.Parked {
		notes = append(notes, stMuted.Render("parked"))
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
		return []hint{{keys.Key(keymap.ListToggle), "toggle"}, {keys.Key(keymap.ListToggleAll), "all"}, {keys.Key(keymap.ListConfirm), "sweep selected"}, {keys.Key(keymap.ListBack), "back"}}
	case sweepDone:
		return []hint{{keys.Key(keymap.ListConfirm) + "/" + keys.Key(keymap.ListBack), "back"}}
	}
	return []hint{{keys.Key(keymap.ListBack), "back"}}
}

func (s *sweepScreen) help() []helpSection {
	return []helpSection{{"Sweep", []hint{
		{keys.Keys(keymap.ListToggle), "select or deselect a branch"},
		{keys.Keys(keymap.ListToggleAll), "select or deselect all"},
		{keys.Keys(keymap.ListConfirm), "sweep the selection: delete branches deleted on origin, free worktrees holding others' branches"},
		{keys.Keys(keymap.ListBack), "back"},
	}}, {"Defaults", []hint{
		{"selected", "anything clean, not parked, and with no Claude session running"},
		{"not selected", "Claude sessions, parked branches, and gone branches with uncommitted changes (selecting discards them)"},
		{"unavailable", "others' branches with uncommitted changes, and anything in the main worktree"},
	}}}
}

func (s *sweepScreen) title() string { return "sweep" }
