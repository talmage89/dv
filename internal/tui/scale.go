package tui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/talmage89/dv/internal/pool"
	"github.com/talmage89/dv/keymap"
)

type scalePhase int

const (
	scaleAdjust scalePhase = iota
	scaleRunning
	scaleDone
)

type scaleStep struct {
	label  string
	add    string    // path to create
	remove pool.Slot // slot to delete (when add is empty)
	result string    // "", "ok", or an error
}

// scaleScreen grows or shrinks the pool. Growing creates detached worktrees at
// the base ref and syncs them; shrinking only ever removes free worktrees.
type scaleScreen struct {
	phase   scalePhase
	target  int
	typed   string // digits typed so far
	steps   []scaleStep
	prepErr string
	report  *pool.SyncReport
}

func newScale(a *App) *scaleScreen { return &scaleScreen{target: len(a.snap.Slots)} }

func (s *scaleScreen) animating() bool { return s.phase == scaleRunning }

func (s *scaleScreen) init(a *App) tea.Cmd {
	// Removal decisions need current statuses, not cached ones.
	var stale []string
	for _, sl := range a.snap.Slots {
		if sl.Detached() && !sl.Missing && !a.fresh[sl.Path] {
			stale = append(stale, sl.Path)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	return a.refreshStatuses(stale)
}

func (s *scaleScreen) plan(a *App) pool.ScalePlan {
	statuses := map[string]pool.Status{}
	for p, st := range a.statuses {
		if a.fresh[p] {
			statuses[p] = st
		}
	}
	return a.repo.PlanScale(a.snap, s.target, statuses, a.live)
}

type scalePrepMsg struct{ err error }

type scaleStepMsg struct {
	i   int
	err error
}

type scaleSyncMsg struct{ report pool.SyncReport }

func (s *scaleScreen) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case scalePrepMsg:
		if msg.err != nil {
			s.prepErr = msg.err.Error()
			s.finish(a)
			return nil
		}
		return s.step(a, 0)
	case scaleStepMsg:
		if msg.err != nil {
			s.steps[msg.i].result = msg.err.Error()
		} else {
			s.steps[msg.i].result = "ok"
		}
		return s.step(a, msg.i+1)
	case scaleSyncMsg:
		s.report = &msg.report
		a.issues = append(a.issues, msg.report.Issues...)
		s.finish(a)
		return tea.Batch(a.reloadSnapshot(), a.refreshStatuses(nil))
	case tea.KeyPressMsg:
		return s.key(a, msg)
	}
	return nil
}

func (s *scaleScreen) key(a *App, msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	switch {
	case keys.Is(k, keymap.ListHelp):
		a.openHelp()
		return nil
	case s.phase == scaleRunning:
		return nil
	case keys.Is(k, keymap.ListBack) || (s.phase == scaleDone && keys.Is(k, keymap.ListConfirm)):
		a.pop()
		return nil
	case s.phase == scaleDone:
		return nil
	}
	switch {
	case keys.Is(k, keymap.ScaleDown):
		s.typed = ""
		s.target = max(s.target-1, 0)
	case keys.Is(k, keymap.ScaleUp):
		s.typed = ""
		s.target = min(s.target+1, pool.MaxSlots)
	case keys.Is(k, keymap.ScaleErase):
		if s.typed != "" {
			s.typed = s.typed[:len(s.typed)-1]
			s.applyTyped(a)
		}
	case keys.Is(k, keymap.ScaleApply):
		return s.start(a)
	default:
		if len(k) == 1 && k[0] >= '0' && k[0] <= '9' && len(s.typed) < 2 {
			s.typed += k
			s.applyTyped(a)
		}
	}
	return nil
}

func (s *scaleScreen) applyTyped(a *App) {
	if n, err := strconv.Atoi(s.typed); err == nil {
		s.target = min(n, pool.MaxSlots)
	} else {
		s.target = len(a.snap.Slots)
	}
}

func (s *scaleScreen) start(a *App) tea.Cmd {
	p := s.plan(a)
	if len(p.Add) == 0 && len(p.Remove) == 0 {
		a.pop()
		return nil
	}
	s.steps = nil
	for _, path := range p.Add {
		s.steps = append(s.steps, scaleStep{label: "create " + filepath.Base(path), add: path})
	}
	for _, sl := range p.Remove {
		s.steps = append(s.steps, scaleStep{label: "remove " + sl.Name, remove: sl})
	}
	run := func() tea.Cmd {
		s.phase = scaleRunning
		a.busy = "scaling"
		if len(p.Add) == 0 {
			return s.step(a, 0)
		}
		repo, ctx := a.repo, a.ctx
		return func() tea.Msg { return scalePrepMsg{err: repo.PrepareAdd(ctx)} }
	}
	if len(p.Remove) > 0 {
		a.confirm("remove "+plural(len(p.Remove), "worktree", "worktrees"), []string{
			"Only free worktrees are removed: detached, no uncommitted changes, no Claude session. Their directories are deleted, including ignored files such as node_modules.",
		}, false, "remove", run)
		return nil
	}
	return run()
}

func (s *scaleScreen) step(a *App, i int) tea.Cmd {
	if i >= len(s.steps) {
		var added []string
		for _, st := range s.steps {
			if st.add != "" && st.result == "ok" {
				added = append(added, st.add)
			}
		}
		if len(added) == 0 {
			s.finish(a)
			return tea.Batch(a.reloadSnapshot(), a.refreshStatuses(nil))
		}
		repo, ctx := a.repo, a.ctx
		return func() tea.Msg { return scaleSyncMsg{report: repo.Sync(ctx, added, pool.SyncAuto)} }
	}
	st, repo, ctx := s.steps[i], a.repo, a.ctx
	return func() tea.Msg {
		var err error
		if st.add != "" {
			err = repo.AddSlot(ctx, st.add)
		} else {
			err = repo.RemoveSlot(ctx, st.remove)
		}
		return scaleStepMsg{i: i, err: err}
	}
}

func (s *scaleScreen) finish(a *App) {
	s.phase = scaleDone
	a.busy = ""
	failed := 0
	for _, st := range s.steps {
		if st.result != "ok" {
			failed++
		}
	}
	switch {
	case s.prepErr != "":
		a.notify(levelError, s.prepErr)
	case failed > 0:
		a.notify(levelError, fmt.Sprintf("%d of %d steps failed", failed, len(s.steps)))
	case s.report != nil && len(s.report.Issues) > 0:
		a.notify(levelWarn, "Worktrees created; sync reported issues")
	default:
		a.notify(levelInfo, "Scaled to "+plural(s.target, "worktree", "worktrees"))
	}
}

func (s *scaleScreen) view(a *App, w, h int) string {
	inner := w - 4
	cur := len(a.snap.Slots)
	var body []string
	if s.phase != scaleAdjust {
		for _, st := range s.steps {
			var icon string
			switch {
			case st.result == "ok":
				icon = stMuted.Render("✓")
			case st.result != "":
				icon = stErr.Render("×")
			case s.phase == scaleRunning:
				icon = a.spinner.View()
			default:
				icon = stMuted.Render("·")
			}
			line := pad(icon, 2) + st.label
			if st.result != "" && st.result != "ok" {
				line += "  " + stErr.Render(st.result)
			}
			body = append(body, truncate(line, inner))
		}
		if s.prepErr != "" {
			body = append(body, "", stErr.Render(s.prepErr))
		}
		if s.report != nil {
			body = append(body, "")
			if len(s.report.Issues) == 0 {
				body = append(body, stMuted.Render("Synced .env and .claude into the new worktrees."))
			}
			for _, is := range s.report.Issues {
				body = append(body, wrap(stWarn.Render(is.String()), inner)...)
			}
		}
		return panel("", body, w, h, true)
	}

	p := s.plan(a)
	inUse, free := 0, 0
	for _, sl := range a.snap.Slots {
		st, ok := a.statuses[sl.Path]
		switch {
		case sl.Missing:
		case sl.Detached() && ok && st.Err == "" && !st.Dirty && a.live[sl.Path] == 0:
			free++
		default:
			inUse++
		}
	}
	count := strconv.Itoa(cur)
	if s.target != cur || s.typed != "" {
		count = stMuted.Render(count+"  →  ") + stBold.Render(strconv.Itoa(s.target))
	}
	body = append(body,
		field("worktrees", true, count),
		field("", false, s.bar(a, p, inner-labelW)),
		field("", false, stMuted.Render(fmt.Sprintf("%d in use · %d free", inUse, free))),
		"",
	)
	switch {
	case len(p.Add) > 0:
		names := make([]string, len(p.Add))
		for i, path := range p.Add {
			names[i] = filepath.Base(path)
		}
		body = append(body, note("Creates "+strings.Join(names, ", ")+" at "+a.repo.Base+", then syncs .env and .claude into them.", inner, plain)...)
	case len(p.Remove) > 0 || p.Shortfall > 0:
		if len(p.Remove) > 0 {
			names := make([]string, len(p.Remove))
			for i, sl := range p.Remove {
				names[i] = sl.Name
			}
			body = append(body, note("Removes "+strings.Join(names, ", ")+".", inner, plain)...)
		}
		if p.Shortfall > 0 {
			body = append(body, note(fmt.Sprintf("Cannot go below %d: the rest hold branches, have changes, or run Claude.", cur-p.Removable), inner, stWarn)...)
		}
	default:
		body = append(body, note("← and → change the count; you can also type it.", inner, stMuted)...)
	}
	if a.statusPending > 0 {
		body = append(body, note("Checking which worktrees are free…", inner, stMuted)...)
	}
	return panel("", body, w, h, true)
}

// bar draws one cell per worktree: in use, free, to be removed, to be added.
func (s *scaleScreen) bar(a *App, p pool.ScalePlan, w int) string {
	removing := map[string]bool{}
	for _, sl := range p.Remove {
		removing[sl.Path] = true
	}
	var b strings.Builder
	cells := 0
	for _, sl := range a.snap.Slots {
		st, ok := a.statuses[sl.Path]
		switch {
		case removing[sl.Path]:
			b.WriteString(stErr.Render("×"))
		case sl.Detached() && ok && st.Err == "" && !st.Dirty && a.live[sl.Path] == 0:
			b.WriteString(stMuted.Render("○"))
		default:
			b.WriteString("●")
		}
		cells++
	}
	for range p.Add {
		b.WriteString(stMuted.Render("+"))
		cells++
	}
	if cells > w {
		return truncate(b.String(), w)
	}
	return b.String()
}

func (s *scaleScreen) hints(a *App) []hint {
	switch s.phase {
	case scaleAdjust:
		return []hint{{keys.Pair(keymap.ScaleDown, keymap.ScaleUp), "adjust"}, {"0-9", "type"}, {keys.Key(keymap.ScaleApply), "apply"}, {keys.Key(keymap.ListBack), "back"}}
	case scaleDone:
		return []hint{{keys.Key(keymap.ListConfirm) + "/" + keys.Key(keymap.ListBack), "back"}}
	}
	return nil
}

func (s *scaleScreen) help() []helpSection {
	return []helpSection{{"Scale", []hint{
		{keys.Keys(keymap.ScaleDown), "lower the target"},
		{keys.Keys(keymap.ScaleUp), "raise the target"},
		{"0-9", "type a target"},
		{keys.Keys(keymap.ScaleApply), "apply"},
		{keys.Keys(keymap.ListBack), "back"},
	}}, {"Rules", []hint{
		{"grow", "new worktrees fill the lowest free numbers, detached at the base ref, then get synced"},
		{"shrink", "only free worktrees are removed: detached, clean, no Claude session"},
	}}}
}

func (s *scaleScreen) title() string { return "scale" }
