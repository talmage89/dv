package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/talmage89/dv/internal/pool"
	"github.com/talmage89/dv/keymap"
)

type rowKind int

const (
	rowCheckedOut rowKind = iota
	rowParked
	rowIdle
	rowRemote // not a local branch: fetch it from origin
)

type row struct {
	kind rowKind
	name string       // full branch name
	b    *pool.Branch // nil for rowRemote
}

// homeScreen is the branch list: a search box over checked-out and parked
// branches, most recently active first. Typing filters; lowercase always goes
// to the search box and Shift+letter runs commands, which is unambiguous
// because branch names are lowercase. A paste arrives as one event, so a
// pasted name can never trigger a command.
type homeScreen struct {
	input  textinput.Model
	cursor int
	sel    string // selected branch, kept across refreshes
	offset int    // first visible list line

	// The list only changes with the snapshot or the query, but it is read
	// several times per frame and the gradient animates at ~12 fps.
	cacheSnap  *pool.Snapshot
	cacheQuery string
	cacheRows  []row
}

func newHome() *homeScreen {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = "search, or paste a branch and press " + keys.Key(keymap.HomeOpen)
	st := textinput.DefaultStyles(true)
	st.Focused.Placeholder = stMuted
	st.Blurred.Placeholder = stMuted
	st.Focused.Text = stBold
	in.SetStyles(st)
	in.Focus()
	return &homeScreen{input: in}
}

func (h *homeScreen) query() string { return strings.TrimSpace(h.input.Value()) }

// rows builds the visible list for the current query.
func (h *homeScreen) rows(a *App) []row {
	if h.cacheSnap == a.snap && h.cacheQuery == h.query() && h.cacheSnap != nil {
		return h.cacheRows
	}
	rows := h.buildRows(a)
	h.cacheSnap, h.cacheQuery, h.cacheRows = a.snap, h.query(), rows
	return rows
}

func (h *homeScreen) buildRows(a *App) []row {
	q := h.query()
	terms := strings.Fields(strings.ToLower(q))
	match := func(name string) bool {
		lower := strings.ToLower(name)
		for _, t := range terms {
			if !strings.Contains(lower, t) {
				return false
			}
		}
		return true
	}
	var rows []row
	for _, b := range a.snap.CheckedOut() {
		if match(b.Name) {
			rows = append(rows, row{kind: rowCheckedOut, name: b.Name, b: b})
		}
	}
	for _, b := range a.snap.Parked() {
		if match(b.Name) {
			rows = append(rows, row{kind: rowParked, name: b.Name, b: b})
		}
	}
	if q == "" {
		return rows
	}
	const maxIdle = 50
	n := 0
	for _, b := range a.snap.Idle() {
		if n < maxIdle && match(b.Name) && b.Name != a.snap.Main.Branch {
			rows = append(rows, row{kind: rowIdle, name: b.Name, b: b})
			n++
		}
	}
	// An exact match jumps to the top, so pasting a
	// branch name and pressing enter always opens that branch.
	exact := -1
	for i, r := range rows {
		if r.name == q {
			exact = i
			break
		}
	}
	if exact > 0 {
		r := rows[exact]
		copy(rows[1:exact+1], rows[:exact])
		rows[0] = r
	}
	if exact < 0 && !strings.ContainsAny(q, " \t") && validRefName(q) && a.snap.Branches[q] == nil {
		rows = append(rows, row{kind: rowRemote, name: q})
	}
	return rows
}

func (h *homeScreen) selected(a *App) (row, bool) {
	rows := h.rows(a)
	h.syncCursor(rows)
	if h.cursor < len(rows) {
		return rows[h.cursor], true
	}
	return row{}, false
}

// syncCursor keeps the selection on the same branch when the list changes.
func (h *homeScreen) syncCursor(rows []row) {
	if h.sel != "" {
		for i, r := range rows {
			if r.name == h.sel {
				h.cursor = i
				return
			}
		}
	}
	h.cursor = min(max(h.cursor, 0), max(len(rows)-1, 0))
	if h.cursor < len(rows) {
		h.sel = rows[h.cursor].name
	}
}

func (h *homeScreen) move(a *App, delta int) {
	rows := h.rows(a)
	if len(rows) == 0 {
		return
	}
	h.syncCursor(rows)
	h.cursor = min(max(h.cursor+delta, 0), len(rows)-1)
	h.sel = rows[h.cursor].name
}

func (h *homeScreen) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		// Keep only the first line, trimmed: copying a branch name from a web
		// page often brings a trailing newline or spaces along.
		text, _, _ := strings.Cut(strings.TrimSpace(msg.Content), "\n")
		msg.Content = strings.TrimSpace(text)
		var cmd tea.Cmd
		h.input, cmd = h.input.Update(msg)
		h.resetSelection()
		return cmd
	case tea.KeyPressMsg:
		return h.key(a, msg)
	}
	var cmd tea.Cmd
	h.input, cmd = h.input.Update(msg)
	return cmd
}

func (h *homeScreen) resetSelection() {
	h.sel = ""
	h.cursor = 0
	h.offset = 0
}

func (h *homeScreen) key(a *App, msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	switch {
	case keys.Is(k, keymap.HomeUp):
		h.move(a, -1)
		return nil
	case keys.Is(k, keymap.HomeDown):
		h.move(a, 1)
		return nil
	case keys.Is(k, keymap.HomePageUp):
		h.move(a, -10)
		return nil
	case keys.Is(k, keymap.HomePageDown):
		h.move(a, 10)
		return nil
	case keys.Is(k, keymap.HomeOpen):
		return h.activate(a)
	case keys.Is(k, keymap.HomeEscape):
		if h.input.Value() != "" {
			h.input.SetValue("")
			h.resetSelection()
			return nil
		}
		return tea.Quit
	case keys.Is(k, keymap.HomeHelp):
		a.openHelp()
		return nil
	case keys.Is(k, keymap.HomeNew):
		return a.push(newNewBranch(a, h.newBranchSeed(a)))
	case keys.Is(k, keymap.HomeFresh):
		return h.fresh(a)
	case keys.Is(k, keymap.HomePrune):
		return a.push(newPrune())
	case keys.Is(k, keymap.HomeSweep):
		return a.push(newSweep())
	case keys.Is(k, keymap.HomeScale):
		return a.push(newScale(a))
	case keys.Is(k, keymap.HomeRun):
		return a.push(newRun())
	case keys.Is(k, keymap.HomeConfig):
		return a.push(newConfigScreen())
	case keys.Is(k, keymap.HomeLog):
		return a.push(newLogScreen())
	case keys.Is(k, keymap.HomeMain):
		return a.open(a.snap.Main.Path)
	case keys.Is(k, keymap.HomeFetch):
		return a.maybeFetch(true)
	case keys.Is(k, keymap.HomePark):
		return h.park(a)
	case keys.Is(k, keymap.HomeDetach):
		return h.release(a)
	case keys.Is(k, keymap.HomeDelete):
		return h.remove(a)
	case keys.Is(k, keymap.HomeCopy):
		return h.copy(a)
	}
	before := h.input.Value()
	var cmd tea.Cmd
	h.input, cmd = h.input.Update(msg)
	if h.input.Value() != before {
		h.resetSelection()
	}
	return cmd
}

// newBranchSeed prefills the new-branch screen with the search text when it
// names a branch that does not exist yet.
func (h *homeScreen) newBranchSeed(a *App) string {
	q := h.query()
	if q != "" && a.snap.Branches[q] == nil && validRefName(q) {
		return q
	}
	return ""
}

func (h *homeScreen) activate(a *App) tea.Cmd {
	r, ok := h.selected(a)
	if !ok {
		return nil
	}
	if r.kind == rowCheckedOut {
		return a.open(r.b.Worktree)
	}
	name := r.name
	desc := "checking out " + name
	if r.kind == rowRemote {
		desc = "fetching " + name
	}
	repo := a.repo
	return a.runOp(desc, func(ctx context.Context) opResult {
		res, err := repo.Checkout(ctx, name)
		if err != nil {
			return opResult{err: friendly(err)}
		}
		return opResult{cd: res.Path, touched: []string{res.Path}, toast: "Checked out " + name}
	})
}

func (h *homeScreen) fresh(a *App) tea.Cmd {
	repo := a.repo
	return a.runOp("opening a fresh worktree", func(ctx context.Context) opResult {
		path, err := repo.Fresh(ctx)
		if err != nil {
			return opResult{err: friendly(err)}
		}
		return opResult{cd: path, touched: []string{path}, toast: "Detached " + filepath.Base(path) + " at " + repo.Base}
	})
}

func (h *homeScreen) park(a *App) tea.Cmd {
	r, ok := h.selected(a)
	if !ok || r.kind == rowRemote {
		return nil
	}
	name, repo := r.name, a.repo
	if r.kind == rowParked {
		return a.runOp("unparking", func(ctx context.Context) opResult {
			if err := repo.Unpark(ctx, name); err != nil {
				return opResult{err: err}
			}
			return opResult{toast: "Unparked " + name + " (search to find it)"}
		})
	}
	do := func() tea.Cmd {
		return a.runOp("parking "+name, func(ctx context.Context) opResult {
			if err := repo.Park(ctx, name); err != nil {
				return opResult{err: friendly(err)}
			}
			res := opResult{toast: "Parked " + name}
			if r.b.Worktree != "" {
				res.touched = []string{r.b.Worktree}
			}
			return res
		})
	}
	if r.kind == rowCheckedOut {
		if err := h.checkReleasable(a, r); err != nil {
			a.notify(levelWarn, err.Error())
			return nil
		}
		if n := a.live[r.b.Worktree]; n > 0 {
			a.confirm("park", []string{stBold.Render(name), "",
				"A Claude session is running in this worktree. Parking detaches it, so the session will find itself on " + a.repo.Base + " instead of " + name + ".",
			}, false, "park anyway", do)
			return nil
		}
	}
	return do()
}

func (h *homeScreen) release(a *App) tea.Cmd {
	r, ok := h.selected(a)
	if !ok || r.kind != rowCheckedOut {
		return nil
	}
	if err := h.checkReleasable(a, r); err != nil {
		a.notify(levelWarn, err.Error())
		return nil
	}
	name, repo, wt := r.name, a.repo, r.b.Worktree
	do := func() tea.Cmd {
		return a.runOp("detaching "+name, func(ctx context.Context) opResult {
			if err := repo.Release(ctx, name); err != nil {
				return opResult{err: friendly(err)}
			}
			return opResult{toast: "Freed the worktree holding " + name + "; the branch is kept", touched: []string{wt}}
		})
	}
	if a.live[wt] > 0 {
		a.confirm("detach", []string{stBold.Render(name), "",
			"A Claude session is running in this worktree. Detaching moves it off " + name + ".",
		}, false, "detach anyway", do)
		return nil
	}
	return do()
}

// checkReleasable refuses early, with a clear message, when the worktree has
// changes. The pool re-checks before acting.
func (h *homeScreen) checkReleasable(a *App, r row) error {
	if r.b.Worktree == a.snap.Main.Path {
		return fmt.Errorf("%s is checked out in the main worktree", r.name)
	}
	if st := a.statuses[r.b.Worktree]; st.Dirty {
		return fmt.Errorf("%s has %s; commit, stash, or discard them first", r.name, plural(st.Files, "uncommitted change", "uncommitted changes"))
	}
	return nil
}

func (h *homeScreen) remove(a *App) tea.Cmd {
	r, ok := h.selected(a)
	if !ok || r.kind == rowRemote {
		return nil
	}
	if r.b.Worktree != "" {
		if err := h.checkReleasable(a, r); err != nil {
			a.notify(levelWarn, err.Error())
			return nil
		}
	}
	name, repo := r.name, a.repo
	safety := repo.BranchSafety(a.ctx, name)
	var lines []string
	danger := false
	switch {
	case safety.Unpushed > 0:
		danger = true
		lines = append(lines, stErr.Render(plural(safety.Unpushed, "commit exists", "commits exist")+" only in this branch and will be lost."))
	case safety.MergedIntoB:
		lines = append(lines, "Every commit is already in "+a.repo.Base+".")
	default:
		lines = append(lines, "Every commit is on origin.")
	}
	if r.b.Worktree != "" {
		lines = append(lines, "Its worktree is freed.")
		if a.live[r.b.Worktree] > 0 {
			lines = append(lines, stWarn.Render("A Claude session is running in that worktree."))
		}
	}
	wt := r.b.Worktree
	a.confirm("delete", append([]string{stBold.Render(name), ""}, lines...), danger, "delete", func() tea.Cmd {
		return a.runOp("deleting "+name, func(ctx context.Context) opResult {
			if err := repo.Delete(ctx, name); err != nil {
				return opResult{err: friendly(err)}
			}
			res := opResult{toast: "Deleted " + name}
			if wt != "" {
				res.touched = []string{wt}
			}
			return res
		})
	})
	return nil
}

func (h *homeScreen) copy(a *App) tea.Cmd {
	r, ok := h.selected(a)
	if !ok {
		return nil
	}
	if runtime.GOOS == "darwin" {
		cmd := exec.Command("pbcopy")
		cmd.Stdin = strings.NewReader(r.name)
		_ = cmd.Run()
	}
	a.notify(levelInfo, "Copied "+r.name)
	return tea.SetClipboard(r.name)
}

// --- view ----------------------------------------------------------------------

// Every row shares one grid: selection marker, age, two status cells
// (uncommitted changes, running Claude session), then the name. Section
// headings, the detail lines, and empty states start at the name column, so
// the screen reads as one aligned column of text.
const (
	ageW    = 4
	nameCol = 2 + ageW + 1 + 2 + 1
)

var nameIndent = strings.Repeat(" ", nameCol)

func (h *homeScreen) view(a *App, w, ht int) string {
	inner := w - 4
	h.input.SetWidth(inner - 3)
	rows := h.rows(a)
	h.syncCursor(rows)

	body := []string{stMuted.Render("/ ") + h.input.View(), ""}
	detail := h.detail(a, rows, inner)
	listH := ht - 1 - len(body) - 1 - len(detail)
	lines, selLine := h.listLines(a, rows, inner)
	if selLine >= 0 {
		if selLine < h.offset {
			h.offset = selLine
		}
		if selLine >= h.offset+listH {
			h.offset = selLine - listH + 1
		}
	}
	h.offset = min(h.offset, max(len(lines)-listH, 0))
	visible := lines[min(h.offset, len(lines)):]
	if len(visible) > listH {
		visible = visible[:listH]
	}
	body = append(body, visible...)
	// The detail follows the list, next to the row it describes.
	if len(detail) > 0 {
		body = append(body, "")
		body = append(body, detail...)
	}
	return panel("", body, w, ht, true)
}

// listLines renders rows with section headings, returning the line index of
// the selected row.
func (h *homeScreen) listLines(a *App, rows []row, w int) ([]string, int) {
	if len(rows) == 0 {
		if h.query() == "" {
			return []string{stMuted.Render(nameIndent + "No branches checked out. Type or paste a branch name, or press " + keys.Key(keymap.HomeNew) + ".")}, -1
		}
		return []string{stMuted.Render(nameIndent + "No matches.")}, -1
	}
	var lines []string
	sel := -1
	prev := rowKind(-1)
	for i, r := range rows {
		if r.kind != prev && r.kind != rowCheckedOut && r.kind != rowRemote {
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			heading := "parked"
			if r.kind == rowIdle {
				heading = "other branches"
			}
			lines = append(lines, stMuted.Render(nameIndent+heading))
		}
		if r.kind == rowRemote && len(lines) > 0 {
			lines = append(lines, "")
		}
		prev = r.kind
		if i == h.cursor {
			sel = len(lines)
		}
		lines = append(lines, h.renderRow(a, r, i == h.cursor, w))
	}
	return lines, sel
}

func (h *homeScreen) renderRow(a *App, r row, selected bool, w int) string {
	bar := "  "
	if selected {
		bar = selMarker() + " "
	}
	name := r.name
	styled := name
	switch {
	case selected:
		styled = rainbow(name)
	case r.kind == rowIdle:
		styled = stMuted.Render(name)
	}
	if r.kind == rowRemote {
		lead := bar + strings.Repeat(" ", ageW+1) + stMuted.Render("+") + "  "
		return lead + truncate(stMuted.Render("fetch ")+styled+stMuted.Render(" from origin"), w-width(lead))
	}
	dirty, live := " ", " "
	if r.b.Worktree != "" {
		st := a.statuses[r.b.Worktree]
		switch {
		case st.Err != "":
			dirty = stWarn.Render("?")
		case st.Dirty:
			dirty = stErr.Render("*")
		}
		if a.live[r.b.Worktree] > 0 {
			live = "◆"
		}
	}
	age := stMuted.Render(fmt.Sprintf("%*s", ageW, ago(r.b.LastActive)))
	lead := bar + age + " " + dirty + live + " "
	if r.b.Gone {
		styled += stWarn.Render("  gone")
	}
	return lead + truncate(styled, w-width(lead))
}

// detail describes the selected branch in two quiet lines under the list:
// its last commit, then its state.
func (h *homeScreen) detail(a *App, rows []row, w int) []string {
	if h.cursor >= len(rows) {
		return nil
	}
	r := rows[h.cursor]
	textW := w - nameCol
	if r.kind == rowRemote {
		return []string{nameIndent + truncate(stMuted.Render("fetches from origin and checks out into a free worktree"), textW)}
	}
	b := r.b
	var lines []string
	if b.Subject != "" {
		lines = append(lines, nameIndent+truncate(b.Subject+stMuted.Render("  "+ago(b.CommitTime)), textW))
	}
	var facts []string
	switch r.kind {
	case rowParked:
		facts = append(facts, "parked")
	case rowIdle:
		facts = append(facts, "not checked out")
	}
	if b.Worktree != "" {
		st, ok := a.statuses[b.Worktree]
		switch {
		case !ok:
			facts = append(facts, "checking…")
		case st.Err != "":
			facts = append(facts, stWarn.Render("status failed: "+firstLine(st.Err)))
		case st.Dirty:
			facts = append(facts, stErr.Render(plural(st.Files, "uncommitted change", "uncommitted changes")))
		default:
			facts = append(facts, "clean")
		}
		if n := a.live[b.Worktree]; n > 0 {
			facts = append(facts, plural(n, "Claude session", "Claude sessions"))
		}
	}
	switch {
	case b.Gone:
		facts = append(facts, stWarn.Render("deleted on origin"))
	case b.Upstream == "":
		facts = append(facts, "not pushed")
	case b.Ahead == 0 && b.Behind == 0:
		facts = append(facts, "pushed")
	default:
		facts = append(facts, fmt.Sprintf("%d to push, %d to pull", b.Ahead, b.Behind))
	}
	if b.HasBase {
		facts = append(facts, fmt.Sprintf("%d ahead, %d behind %s", b.BaseAhead, b.BaseBehind, shortRef(a.repo.Base)))
	}
	if t := clock(b.LastActive); t != "" {
		facts = append(facts, "active "+t)
	}
	lines = append(lines, nameIndent+truncate(stMuted.Render(strings.Join(facts, " · ")), textW))
	return lines
}

// shortRef drops the remote from a ref: origin/main → main.
func shortRef(ref string) string {
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func (h *homeScreen) status(a *App) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("%d checked out", len(a.snap.CheckedOut())))
	free, known := a.freeCount()
	switch {
	case !known:
		parts = append(parts, "counting free…")
	case free == 0:
		parts = append(parts, stWarn.Render("no free worktrees")+stMuted.Render(", "+keys.Key(keymap.HomeScale)+" adds more"))
	default:
		parts = append(parts, fmt.Sprintf("%d free", free))
	}
	if n := len(a.snap.Parked()); n > 0 {
		parts = append(parts, fmt.Sprintf("%d parked", n))
	}
	if total, _ := a.goneCounts(); total > 0 {
		parts = append(parts, stWarn.Render(plural(total, "branch", "branches")+" deleted on origin")+stMuted.Render(", "+keys.Key(keymap.HomePrune)+" prunes"))
	}
	return stMuted.Render(strings.Join(parts, stMuted.Render(" · ")))
}

func (h *homeScreen) hints(a *App) []hint {
	return []hint{
		{keys.Key(keymap.HomeOpen), "open"},
		{keys.Key(keymap.HomeNew), "new"},
		{keys.Key(keymap.HomeFresh), "fresh"},
		{keys.Key(keymap.HomePark), "park"},
		{keys.Key(keymap.HomePrune), "prune"},
		{keys.Key(keymap.HomeSweep), "sweep"},
		{keys.Key(keymap.HomeRun), "run"},
		{keys.Key(keymap.HomeScale), "scale"},
		{keys.Key(keymap.HomeMain), "main"},
		{keys.Key(keymap.HomeConfig), "config"},
		{keys.Key(keymap.HomeHelp), "help"},
	}
}

func (h *homeScreen) help() []helpSection {
	return []helpSection{
		{"Search", []hint{
			{"type / paste", "filter branches; paste a full name to find any branch"},
			{keys.Keys(keymap.HomeOpen), "open the selected branch, checking it out into a free worktree if needed"},
			{keys.Keys(keymap.HomeUp), "move up"},
			{keys.Keys(keymap.HomeDown), "move down"},
			{keys.Pair(keymap.HomePageUp, keymap.HomePageDown), "move a page"},
			{keys.Keys(keymap.HomeEscape), "clear the search, or quit"},
		}},
		{"Branch", []hint{
			{keys.Keys(keymap.HomePark), "park (free its worktree, keep it listed) / unpark"},
			{keys.Keys(keymap.HomeDetach), "detach: free its worktree without parking"},
			{keys.Keys(keymap.HomeDelete), "delete the branch"},
			{keys.Keys(keymap.HomeCopy), "copy the branch name"},
		}},
		{"Go to", []hint{
			{keys.Keys(keymap.HomeNew), "new branch"},
			{keys.Keys(keymap.HomeFresh), "fresh worktree: a free one, detached at the base ref"},
			{keys.Keys(keymap.HomeMain), "main worktree"},
			{keys.Keys(keymap.HomePrune), "prune branches deleted on origin"},
			{keys.Keys(keymap.HomeSweep), "sweep: free worktrees holding branches with no commit of yours"},
			{keys.Keys(keymap.HomeRun), "run a command in every worktree"},
			{keys.Keys(keymap.HomeScale), "scale the number of worktrees"},
			{keys.Keys(keymap.HomeConfig), "config"},
			{keys.Keys(keymap.HomeLog), "log and sync issues"},
			{keys.Keys(keymap.HomeFetch), "fetch origin now"},
		}},
	}
}

// friendly rewrites pool errors into what to do about them.
func friendly(err error) error {
	switch {
	case errors.Is(err, pool.ErrNoFreeSlot):
		return fmt.Errorf("no free worktree — press %s to add more, %s to prune merged branches, or %s to sweep others' branches", keys.Key(keymap.HomeScale), keys.Key(keymap.HomePrune), keys.Key(keymap.HomeSweep))
	case errors.Is(err, pool.ErrDirty):
		return fmt.Errorf("%w — commit, stash, or discard first", err)
	}
	return err
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
