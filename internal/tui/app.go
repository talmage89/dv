// Package tui is dv's full-screen interface.
package tui

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/talmage89/dv/internal/config"
	"github.com/talmage89/dv/internal/pool"
	"github.com/talmage89/dv/keymap"
)

// Options configure the TUI.
type Options struct {
	Version string
	// CanCD is false when dv runs without the shell integration, in which case
	// opening a worktree cannot change the shell's directory.
	CanCD bool
}

// Result is what the TUI hands back to the caller when it exits.
type Result struct {
	CD   string // directory the shell should change into, if any
	Note string // what happened, for a line on stderr after the screen clears
}

// Run starts the TUI and blocks until it exits.
func Run(ctx context.Context, cfg *config.Config, repo *pool.Repo, opts Options, progOpts ...tea.ProgramOption) (Result, error) {
	app, err := newApp(ctx, cfg, repo, opts)
	if err != nil {
		return Result{}, err
	}
	defer app.cancel()
	p := tea.NewProgram(app, append([]tea.ProgramOption{tea.WithContext(ctx)}, progOpts...)...)
	if _, err := p.Run(); err != nil {
		return Result{}, err
	}
	app.repo.SaveCache(pool.Cache{Statuses: app.statuses, Live: app.live})
	return Result{CD: app.cdTarget, Note: app.exitNote}, nil
}

// App is the root model. Screens hold a pointer to it and read shared state
// (snapshot, statuses, live sessions) from it; all mutation happens in Update.
type App struct {
	ctx    context.Context
	cancel context.CancelFunc
	opts   Options
	cfg    *config.Config
	repo   *pool.Repo
	home   string // $HOME, for abbreviating paths

	snap     *pool.Snapshot
	statuses map[string]pool.Status
	fresh    map[string]bool // statuses computed during this session
	live     map[string]int
	issues   []pool.SyncIssue

	w, h    int
	stack   []screen
	modal   *modal
	toast   *toast
	log     []logEntry
	spinner spinner.Model

	busy           string // description of the running mutating operation
	fetching       bool
	lastFetch      time.Time
	fetchErr       string
	statusPending  int
	lastStatusPass time.Time
	syncing        bool

	cdTarget  string
	exitNote  string
	spinning  bool
	animating bool // the gradient animation loop is scheduled
	blurred   bool // the terminal reported losing focus
}

func newApp(ctx context.Context, cfg *config.Config, repo *pool.Repo, opts Options) (*App, error) {
	snap, err := repo.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	cache := repo.LoadCache()
	ctx, cancel := context.WithCancel(ctx)
	home, _ := os.UserHomeDir()
	a := &App{
		ctx:      ctx,
		cancel:   cancel,
		opts:     opts,
		cfg:      cfg,
		repo:     repo,
		home:     home,
		snap:     snap,
		statuses: cache.Statuses,
		fresh:    map[string]bool{},
		live:     cache.Live,
		spinner:  spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(plain)),
	}
	a.pruneCache()
	for _, warning := range keys.Warnings {
		a.logf(levelWarn, "keymap: %s", warning)
	}
	a.stack = []screen{newHome()}
	return a, nil
}

// pruneCache drops cached entries for worktrees that no longer exist.
func (a *App) pruneCache() {
	valid := map[string]bool{}
	for _, s := range a.snap.All() {
		valid[s.Path] = true
	}
	for p := range a.statuses {
		if !valid[p] {
			delete(a.statuses, p)
		}
	}
	for p := range a.live {
		if !valid[p] {
			delete(a.live, p)
		}
	}
}

func (a *App) Init() tea.Cmd {
	return tea.Batch(
		a.loadLive(),
		a.refreshStatuses(nil),
		a.maybeFetch(false),
		a.syncCmd(pool.SyncAuto),
		tick(1),
		a.startAnimation(),
	)
}

// animFrame paces the selection gradient: smooth enough to read as motion,
// slow enough to stay cheap. It stops while the terminal is unfocused.
const animFrame = 80 * time.Millisecond

type animMsg struct{}

func animate() tea.Cmd {
	return tea.Tick(animFrame, func(time.Time) tea.Msg { return animMsg{} })
}

func (a *App) startAnimation() tea.Cmd {
	setPalette(a.cfg.String(config.KeyPalette))
	if gradientStill || a.animating {
		return nil
	}
	a.animating = true
	return animate()
}

func (a *App) top() screen { return a.stack[len(a.stack)-1] }

func (a *App) push(s screen) tea.Cmd {
	a.stack = append(a.stack, s)
	if i, ok := s.(initializer); ok {
		return i.init(a)
	}
	return nil
}

func (a *App) pop() {
	if len(a.stack) > 1 {
		a.stack = a.stack[:len(a.stack)-1]
	}
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := a.update(msg)
	// The spinner only ticks while something is in flight, so an idle
	// screen does not redraw ten times a second.
	if !a.spinning && a.active() {
		a.spinning = true
		cmd = tea.Batch(cmd, a.spinner.Tick)
	}
	return m, cmd
}

// active reports whether anything shown with a spinner is in progress.
func (a *App) active() bool {
	if a.busy != "" || a.fetching || a.statusPending > 0 || a.syncing {
		return true
	}
	if an, ok := a.top().(interface{ animating() bool }); ok {
		return an.animating()
	}
	return false
}

func (a *App) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.w, a.h = msg.Width, msg.Height
	case spinner.TickMsg:
		if !a.active() {
			a.spinning = false
			return a, nil
		}
		var cmd tea.Cmd
		a.spinner, cmd = a.spinner.Update(msg)
		return a, cmd
	case tickMsg:
		if a.toast != nil && time.Now().After(a.toast.until) {
			a.toast = nil
		}
		cmds := []tea.Cmd{tick(msg.n + 1)}
		if msg.n%5 == 0 && a.busy == "" {
			cmds = append(cmds, a.reloadSnapshot(), a.loadLive())
		}
		return a, tea.Batch(cmds...)
	case animMsg:
		if a.blurred || gradientStill {
			a.animating = false
			return a, nil
		}
		gradientShift = math.Mod(gradientShift+gradientSpeed*animFrame.Seconds(), gradientPeriod*1000)
		return a, animate()
	case tea.BlurMsg:
		a.blurred = true // the animation loop stops at its next frame
	case tea.FocusMsg:
		// Coming back to the terminal is the moment state is most likely
		// stale. Multiplexers report focus on every pane switch, so the
		// expensive status pass is throttled.
		a.blurred = false
		cmds := []tea.Cmd{a.reloadSnapshot(), a.loadLive()}
		cmds = append(cmds, a.startAnimation())
		if time.Since(a.lastStatusPass) > 30*time.Second {
			cmds = append(cmds, a.refreshStatuses(nil))
		}
		return a, tea.Batch(cmds...)
	case snapshotMsg:
		if msg.err != nil {
			a.logf(levelError, "reload: %v", msg.err)
		} else {
			a.snap = msg.snap
		}
	case statusMsg:
		a.statuses[msg.path] = msg.st
		a.fresh[msg.path] = true
		if msg.next != nil {
			return a, tea.Batch(msg.next, a.forward(msg))
		}
	case statusDoneMsg:
		a.statusPending--
	case liveMsg:
		a.live = msg.live
	case fetchDoneMsg:
		a.fetching = false
		a.lastFetch = time.Now()
		if msg.err != nil {
			a.fetchErr = msg.err.Error()
			a.logf(levelWarn, "%v", msg.err)
			return a, a.forward(msg)
		}
		a.fetchErr = ""
		a.logf(levelInfo, "fetched origin")
		return a, tea.Batch(a.reloadSnapshot(), a.forward(msg))
	case syncDoneMsg:
		a.syncing = false
		a.issues = msg.report.Issues
		for _, c := range msg.report.Changes {
			a.logf(levelInfo, "sync %s", c)
		}
		for _, i := range msg.report.Issues {
			lvl := levelWarn
			if i.Severity == pool.SeverityError {
				lvl = levelError
			}
			a.logf(lvl, "sync %s", i)
		}
		if msg.force && len(msg.report.Issues) == 0 {
			a.notify(levelInfo, fmt.Sprintf("Synced (%s)", plural(len(msg.report.Changes), "change", "changes")))
		}
	case opDoneMsg:
		return a, a.opDone(msg)
	case tea.KeyPressMsg:
		if a.modal != nil {
			return a, a.modal.update(a, msg)
		}
		if msg.String() == "ctrl+c" {
			if q, ok := a.top().(interrupter); ok {
				return a, q.interrupt(a)
			}
			return a, tea.Quit
		}
	}
	return a, a.forward(msg)
}

// forward passes msg to the active screen.
func (a *App) forward(msg tea.Msg) tea.Cmd {
	return a.top().update(a, msg)
}

func (a *App) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.ReportFocus = true
	v.WindowTitle = "dv · " + a.repo.Name()
	if a.w == 0 || a.h == 0 {
		return v
	}
	if a.w < 40 || a.h < 12 {
		v.SetContent(center(stMuted.Render("dv needs a bigger terminal"), a.w, a.h))
		return v
	}
	v.SetContent(a.frame())
	return v
}

// maxWidth caps the content column so nothing drifts to the far edge of a
// wide terminal.
const maxWidth = 120

// frame lays the screen out top-down: header, the screen's content, then the
// status line and key hints right under it. Space left over stays empty at the
// bottom rather than separating the hints from what they describe.
func (a *App) frame() string {
	cw := min(a.w, maxWidth)
	header := a.renderHeader(cw)
	bodyH := a.h - len(header) - 3 // blank row, status, hints
	var body string
	if a.modal != nil {
		body = a.modal.view(cw, bodyH)
	} else {
		body = a.top().view(a, cw, bodyH)
	}
	lines := strings.Split(body, "\n")
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	out := append([]string{}, header...)
	out = append(out, lines...)
	out = append(out, "", a.renderStatus(cw), renderHints(a.hints(), cw))
	for len(out) < a.h {
		out = append(out, "")
	}
	for i, l := range out {
		out[i] = pad(l, a.w)
	}
	return strings.Join(out[:a.h], "\n")
}

func (a *App) hints() []hint {
	if a.modal != nil {
		return a.modal.hints()
	}
	return a.top().hints(a)
}

// renderHeader is a breadcrumb (dv, the repo, the current screen) with
// anything that needs attention on the right. When nothing does, the right
// side stays empty.
func (a *App) renderHeader(w int) []string {
	title := stBold.Render("dv") + "  " + a.repo.Name()
	if t, ok := a.top().(titled); ok {
		title += stMuted.Render("  ›  ") + t.title()
	}
	if a.modal != nil && !a.modal.confirm {
		title += stMuted.Render("  ›  ") + a.modal.title
	}
	var right []string
	base := shortRef(a.repo.Base)
	st := a.statuses[a.snap.Main.Path]
	switch {
	case a.snap.Main.Branch != base:
		label := a.snap.Main.Branch
		if label == "" {
			label = "detached"
		}
		right = append(right, stMuted.Render("main worktree on ")+label)
	case st.Dirty:
		right = append(right, stMuted.Render("main worktree has changes"))
	}
	if !a.opts.CanCD {
		right = append(right, stWarn.Render("no shell integration"))
	}
	inner := w - 4
	line := spread(title, strings.Join(right, stMuted.Render("  ·  ")), inner)
	return []string{"  " + pad(line, inner) + "  ", ""}
}

// renderStatus draws the line above the key hints: a toast if one is showing,
// otherwise the screen's own status, with background activity on the right.
func (a *App) renderStatus(w int) string {
	left := ""
	if a.toast != nil {
		left = a.toast.render()
	} else if s, ok := a.top().(statusProvider); ok {
		left = s.status(a)
	}
	var right []string
	switch {
	case a.busy != "":
		right = append(right, a.spinner.View()+" "+a.busy)
	case a.fetching:
		right = append(right, a.spinner.View()+" fetching")
	case a.statusPending > 0:
		right = append(right, a.spinner.View()+" checking worktrees")
	case a.syncing:
		right = append(right, a.spinner.View()+" syncing")
	}
	if n := len(a.issues); n > 0 {
		right = append(right, stWarn.Render(plural(n, "sync issue", "sync issues"))+stMuted.Render(", "+keys.Key(keymap.HomeLog)+" shows"))
	} else if a.fetchErr != "" && !a.fetching {
		right = append(right, stWarn.Render("fetch failed")+stMuted.Render(", "+keys.Key(keymap.HomeLog)+" shows"))
	}
	return spread("  "+left, strings.Join(right, "  ")+"  ", w)
}

// notify shows a transient message in the status line and records it.
func (a *App) notify(lvl level, text string) {
	a.toast = &toast{text: text, level: lvl, until: time.Now().Add(5 * time.Second)}
	a.logf(lvl, "%s", text)
}

// open quits the TUI, asking the shell to change into path.
func (a *App) open(path string) tea.Cmd {
	if !a.opts.CanCD {
		a.notify(levelWarn, "Shell integration is not installed, so dv cannot change your directory. Run `dv install`.")
		return nil
	}
	a.cdTarget = path
	return tea.Quit
}

// --- background work ---------------------------------------------------------

type tickMsg struct{ n int }

func tick(n int) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{n: n} })
}

type snapshotMsg struct {
	snap *pool.Snapshot
	err  error
}

func (a *App) reloadSnapshot() tea.Cmd {
	repo, ctx := a.repo, a.ctx
	return func() tea.Msg {
		s, err := repo.Snapshot(ctx)
		return snapshotMsg{snap: s, err: err}
	}
}

type liveMsg struct{ live map[string]int }

func (a *App) loadLive() tea.Cmd {
	var paths []string
	for _, s := range a.snap.All() {
		paths = append(paths, s.Path)
	}
	ctx := a.ctx
	return func() tea.Msg { return liveMsg{live: pool.LiveSessions(ctx, paths)} }
}

type statusMsg struct {
	path string
	st   pool.Status
	next tea.Cmd
}

type statusDoneMsg struct{}

// refreshStatuses recomputes `git status` for paths (all worktrees when nil),
// streaming results so dirty markers fill in as they arrive. Worktrees holding
// recently active branches go first.
func (a *App) refreshStatuses(paths []string) tea.Cmd {
	if paths == nil {
		paths = a.statusOrder()
		a.lastStatusPass = time.Now()
	}
	if len(paths) == 0 {
		return nil
	}
	a.statusPending++
	ch := make(chan statusMsg, len(paths))
	ctx := a.ctx
	go func() {
		pool.StatusAll(ctx, paths, func(p string, st pool.Status) { ch <- statusMsg{path: p, st: st} })
		close(ch)
	}()
	var wait tea.Cmd
	wait = func() tea.Msg {
		m, ok := <-ch
		if !ok {
			return statusDoneMsg{}
		}
		m.next = wait
		return m
	}
	return wait
}

func (a *App) statusOrder() []string {
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, b := range a.snap.CheckedOut() {
		add(b.Worktree)
	}
	add(a.snap.Main.Path)
	for _, s := range a.snap.Slots {
		if !s.Missing {
			add(s.Path)
		}
	}
	return paths
}

type fetchDoneMsg struct{ err error }

// maybeFetch fetches origin in the background unless it was fetched in the
// last minute (by dv or anything else) and force is false.
func (a *App) maybeFetch(force bool) tea.Cmd {
	if a.fetching {
		return nil
	}
	if !force {
		if info, err := os.Stat(filepath.Join(a.repo.CommonDir, "FETCH_HEAD")); err == nil && time.Since(info.ModTime()) < time.Minute {
			a.lastFetch = info.ModTime()
			return nil
		}
	}
	a.fetching = true
	repo := a.repo
	ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
	return func() tea.Msg {
		defer cancel()
		return fetchDoneMsg{err: repo.Fetch(ctx)}
	}
}

type syncDoneMsg struct {
	report pool.SyncReport
	force  bool
}

// syncCmd syncs .env and .claude across main and every pool slot.
func (a *App) syncCmd(mode pool.SyncMode) tea.Cmd {
	return a.syncPaths(a.syncTargets(), mode)
}

func (a *App) syncTargets() []string {
	paths := []string{a.snap.Main.Path}
	for _, s := range a.snap.Slots {
		if !s.Missing {
			paths = append(paths, s.Path)
		}
	}
	return paths
}

func (a *App) syncPaths(paths []string, mode pool.SyncMode) tea.Cmd {
	if a.syncing {
		return nil
	}
	a.syncing = true
	repo, ctx := a.repo, a.ctx
	return func() tea.Msg {
		return syncDoneMsg{report: repo.Sync(ctx, paths, mode), force: mode == pool.SyncForce}
	}
}

// --- mutating operations -------------------------------------------------------

// opResult is what a mutating operation reports back.
type opResult struct {
	toast   string
	err     error
	cd      string   // open this directory (quits the TUI)
	touched []string // worktrees whose status should be recomputed
	after   func(a *App) tea.Cmd
}

type opDoneMsg struct {
	desc string
	res  opResult
}

// runOp runs fn as the single in-flight mutating operation. Operations are
// serialized so two of them can never race for the same free worktree.
func (a *App) runOp(desc string, fn func(ctx context.Context) opResult) tea.Cmd {
	if a.busy != "" {
		a.notify(levelWarn, "Still "+a.busy+"…")
		return nil
	}
	a.busy = desc
	ctx := a.ctx
	return func() tea.Msg { return opDoneMsg{desc: desc, res: fn(ctx)} }
}

func (a *App) opDone(msg opDoneMsg) tea.Cmd {
	a.busy = ""
	res := msg.res
	cmds := []tea.Cmd{a.reloadSnapshot(), a.loadLive()}
	if res.err != nil {
		a.notify(levelError, res.err.Error())
	} else if res.toast != "" {
		a.notify(levelInfo, res.toast)
	}
	if len(res.touched) > 0 {
		cmds = append(cmds, a.refreshStatuses(res.touched))
	}
	if res.after != nil {
		cmds = append(cmds, res.after(a))
	}
	if res.err == nil && res.cd != "" {
		a.exitNote = res.toast
		return a.open(res.cd)
	}
	cmds = append(cmds, a.forward(msg))
	return tea.Batch(cmds...)
}

// --- shared state helpers ----------------------------------------------------

// freeCount returns how many pool slots are free according to the latest
// statuses. Slots whose status is unknown are not counted.
func (a *App) freeCount() (free int, known bool) {
	known = true
	for _, s := range a.snap.Slots {
		if s.Missing || !s.Detached() || a.live[s.Path] > 0 {
			continue
		}
		st, ok := a.statuses[s.Path]
		if !ok {
			known = false
			continue
		}
		if st.Err == "" && !st.Dirty {
			free++
		}
	}
	return free, known
}

// goneHeld counts branches with a deleted upstream, and how many of them hold
// a worktree.
func (a *App) goneCounts() (total, holding int) {
	for _, b := range a.snap.Gone() {
		total++
		if b.Worktree != "" && b.Worktree != a.snap.Main.Path {
			holding++
		}
	}
	return total, holding
}

// slotLabel names a worktree for places that must identify one without a
// branch (run output, scale plans).
func (a *App) slotLabel(s pool.Slot) string {
	switch {
	case s.Main:
		return "main"
	case s.Branch != "":
		return s.Branch
	default:
		return s.Name
	}
}

// sortedIssues returns sync issues grouped by worktree.
func (a *App) sortedIssues() []pool.SyncIssue {
	out := append([]pool.SyncIssue(nil), a.issues...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Worktree < out[j].Worktree })
	return out
}
