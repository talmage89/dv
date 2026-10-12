package tui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/talmage89/dv/internal/config"
	"github.com/talmage89/dv/internal/pool"
	"github.com/talmage89/dv/keymap"
)

type jobState int

const (
	jobQueued jobState = iota
	jobRunning
	jobOK
	jobFailed
	jobStopped
)

type job struct {
	label  string
	slot   pool.Slot
	state  jobState
	exit   int
	errMsg string
	start  time.Time
	end    time.Time
	out    outBuf
	cmd    *exec.Cmd
	stop   bool // the user asked this job to stop
}

func (j *job) elapsed() time.Duration {
	switch {
	case j.start.IsZero():
		return 0
	case j.end.IsZero():
		return time.Since(j.start)
	default:
		return j.end.Sub(j.start)
	}
}

func (j *job) done() bool { return j.state >= jobOK }

type runScope int

const (
	scopeAll runScope = iota
	scopeBranches
)

// runScreen runs one shell command in many worktrees at once and shows each
// worktree's output separately.
type runScreen struct {
	composing bool
	input     textinput.Model
	scope     runScope
	history   []string
	histIdx   int // len(history) when not browsing

	command string
	jobs    []*job
	events  chan runEvent
	cursor  int
	scroll  int // lines up from the bottom; 0 follows new output
	failed  bool
	zoom    bool
}

func newRun() *runScreen {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = "command to run in each worktree, e.g. git status -sb"
	st := textinput.DefaultStyles(true)
	st.Focused.Placeholder = stMuted
	st.Focused.Text = stBold
	in.SetStyles(st)
	in.Focus()
	h := loadHistory()
	return &runScreen{composing: true, input: in, history: h, histIdx: len(h)}
}

func (s *runScreen) targets(a *App) []pool.Slot {
	var out []pool.Slot
	out = append(out, a.snap.Main)
	for _, sl := range a.snap.Slots {
		if sl.Missing || (s.scope == scopeBranches && sl.Detached()) {
			continue
		}
		out = append(out, sl)
	}
	if s.scope == scopeBranches {
		for _, sl := range a.snap.External {
			if !sl.Detached() && !sl.Missing {
				out = append(out, sl)
			}
		}
	}
	return out
}

func (s *runScreen) animating() bool { return s.running() }

func (s *runScreen) running() bool {
	for _, j := range s.jobs {
		if j.state == jobRunning || j.state == jobQueued {
			return true
		}
	}
	return false
}

// --- events ----------------------------------------------------------------------

type runEvent struct {
	job  int
	data []byte
	exit *exitInfo
}

type exitInfo struct {
	code int
	err  string
}

type runEventMsg struct{ ev runEvent }

func (s *runScreen) wait() tea.Cmd {
	ch := s.events
	return func() tea.Msg { return runEventMsg{ev: <-ch} }
}

func (s *runScreen) start(a *App, command string, only []int) tea.Cmd {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	s.command = command
	saveHistory(s.history, command)
	s.history = loadHistory()
	s.histIdx = len(s.history)
	if only == nil {
		s.jobs = nil
		for _, sl := range s.targets(a) {
			s.jobs = append(s.jobs, &job{label: a.slotLabel(sl), slot: sl})
		}
		s.cursor = 0
	} else {
		for _, i := range only {
			j := s.jobs[i]
			*j = job{label: j.label, slot: j.slot}
		}
	}
	s.composing = false
	s.scroll = 0
	s.failed = false
	if s.events == nil {
		s.events = make(chan runEvent, 256)
	}
	s.schedule(a)
	return s.wait()
}

// schedule starts queued jobs up to the configured parallelism.
func (s *runScreen) schedule(a *App) {
	limit := a.cfg.Int(config.KeyParallelism)
	running := 0
	for _, j := range s.jobs {
		if j.state == jobRunning {
			running++
		}
	}
	for i, j := range s.jobs {
		if running >= limit {
			break
		}
		if j.state == jobQueued {
			s.launch(a, i)
			running++
		}
	}
}

func (s *runScreen) launch(a *App, i int) {
	j := s.jobs[i]
	j.state = jobRunning
	j.start = time.Now()
	shell := "sh"
	if p, err := exec.LookPath("bash"); err == nil {
		shell = p
	}
	cmd := exec.Command(shell, "-c", s.command)
	cmd.Dir = j.slot.Path
	cmd.Env = append(os.Environ(),
		"DV_WORKTREE="+j.slot.Path,
		"DV_BRANCH="+j.slot.Branch,
		"DV_MAIN="+a.repo.Main,
		"DV_ROOT="+a.repo.Root,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A backgrounded grandchild can hold the output pipe open forever; stop
	// waiting for it shortly after the command itself exits.
	cmd.WaitDelay = 2 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	events := s.events
	if err := cmd.Start(); err != nil {
		j.state = jobFailed
		j.errMsg = err.Error()
		j.end = time.Now()
		return
	}
	j.cmd = cmd
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				events <- runEvent{job: i, data: append([]byte(nil), buf[:n]...)}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		pw.Close()
		<-readDone // deliver all output before the exit event
		info := &exitInfo{code: cmd.ProcessState.ExitCode()}
		if err != nil && info.code < 0 {
			info.err = err.Error()
		}
		events <- runEvent{job: i, exit: info}
	}()
}

// stop interrupts every running job's process group, escalating to SIGKILL.
func (s *runScreen) stop() {
	for _, j := range s.jobs {
		switch j.state {
		case jobQueued:
			j.state = jobStopped
		case jobRunning:
			j.stop = true
			if j.cmd != nil && j.cmd.Process != nil {
				pgid := j.cmd.Process.Pid
				_ = syscall.Kill(-pgid, syscall.SIGINT)
				time.AfterFunc(3*time.Second, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
			}
		}
	}
}

func (s *runScreen) interrupt(a *App) tea.Cmd {
	if s.running() {
		s.stop()
		a.notify(levelWarn, "Stopping…")
		return nil
	}
	return tea.Quit
}

// --- update ----------------------------------------------------------------------

func (s *runScreen) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case runEventMsg:
		ev := msg.ev
		if ev.job >= len(s.jobs) {
			return s.wait()
		}
		j := s.jobs[ev.job]
		if ev.data != nil {
			j.out.write(ev.data)
		}
		if ev.exit != nil {
			j.end = time.Now()
			j.exit = ev.exit.code
			j.errMsg = ev.exit.err
			switch {
			case ev.exit.code == 0:
				j.state = jobOK
			case j.stop:
				j.state = jobStopped
			default:
				j.state = jobFailed
			}
			j.cmd = nil
			s.schedule(a)
			if !s.running() {
				s.finished(a)
			}
		}
		return s.wait()
	case tea.PasteMsg:
		if s.composing {
			text, _, _ := strings.Cut(strings.TrimSpace(msg.Content), "\n")
			msg.Content = text
			var cmd tea.Cmd
			s.input, cmd = s.input.Update(msg)
			return cmd
		}
	case tea.KeyPressMsg:
		if s.composing {
			return s.composeKey(a, msg)
		}
		return s.outputKey(a, msg)
	}
	if s.composing {
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return cmd
	}
	return nil
}

func (s *runScreen) finished(a *App) {
	ok, failed := 0, 0
	for _, j := range s.jobs {
		switch j.state {
		case jobOK:
			ok++
		case jobFailed, jobStopped:
			failed++
		}
	}
	if failed > 0 {
		a.notify(levelError, fmt.Sprintf("%s: %d succeeded, %d failed", s.command, ok, failed))
	} else {
		a.notify(levelInfo, fmt.Sprintf("%s: succeeded in %s", s.command, plural(ok, "worktree", "worktrees")))
	}
}

func (s *runScreen) composeKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	switch k := msg.String(); {
	case keys.Is(k, keymap.TextCancel):
		if len(s.jobs) > 0 {
			s.composing = false
			return nil
		}
		a.pop()
		return nil
	case keys.Is(k, keymap.TextSubmit):
		return s.start(a, s.input.Value(), nil)
	case keys.Is(k, keymap.RunWhere):
		s.scope = 1 - s.scope
		return nil
	case keys.Is(k, keymap.RunHistoryPrev):
		if s.histIdx > 0 {
			s.histIdx--
			s.input.SetValue(s.history[s.histIdx])
			s.input.CursorEnd()
		}
		return nil
	case keys.Is(k, keymap.RunHistoryNext):
		if s.histIdx < len(s.history)-1 {
			s.histIdx++
			s.input.SetValue(s.history[s.histIdx])
		} else {
			s.histIdx = len(s.history)
			s.input.SetValue("")
		}
		s.input.CursorEnd()
		return nil
	}
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	return cmd
}

func (s *runScreen) visibleJobs() []int {
	var idx []int
	for i, j := range s.jobs {
		if !s.failed || j.state == jobFailed || j.state == jobStopped {
			idx = append(idx, i)
		}
	}
	return idx
}

func (s *runScreen) outputKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	vis := s.visibleJobs()
	pos := 0
	for i, idx := range vis {
		if idx == s.cursor {
			pos = i
		}
	}
	page := max(a.h-10, 1) / 2
	switch k := msg.String(); {
	case keys.Is(k, keymap.ListHelp):
		a.openHelp()
	case keys.Is(k, keymap.ListUp):
		if pos > 0 {
			s.cursor = vis[pos-1]
			s.scroll = 0
		}
	case keys.Is(k, keymap.ListDown):
		if pos < len(vis)-1 {
			s.cursor = vis[pos+1]
			s.scroll = 0
		}
	case keys.Is(k, keymap.RunPageUp):
		s.scroll += page
	case keys.Is(k, keymap.RunPageDown):
		s.scroll = max(s.scroll-page, 0)
	case keys.Is(k, keymap.RunTop):
		s.scroll = 1 << 30
	case keys.Is(k, keymap.RunBottom):
		s.scroll = 0
	case keys.Is(k, keymap.RunZoom):
		s.zoom = !s.zoom
	case keys.Is(k, keymap.RunFailedOnly):
		s.failed = !s.failed
		if vis := s.visibleJobs(); len(vis) > 0 {
			found := false
			for _, i := range vis {
				found = found || i == s.cursor
			}
			if !found {
				s.cursor = vis[0]
			}
		}
	case keys.Is(k, keymap.RunRerunFailed):
		if s.running() {
			return nil
		}
		var redo []int
		for i, j := range s.jobs {
			if j.state == jobFailed || j.state == jobStopped {
				redo = append(redo, i)
			}
		}
		if len(redo) == 0 {
			a.notify(levelInfo, "Nothing failed")
			return nil
		}
		return s.start(a, s.command, redo)
	case keys.Is(k, keymap.RunRerunAll):
		if s.running() {
			return nil
		}
		return s.start(a, s.command, nil)
	case keys.Is(k, keymap.RunStop):
		if s.running() {
			s.stop()
			a.notify(levelWarn, "Stopping…")
		}
	case keys.Is(k, keymap.RunEdit):
		if s.running() {
			a.notify(levelWarn, "Still running; press "+keys.Key(keymap.RunStop)+" to stop first")
			return nil
		}
		s.input.SetValue(s.command)
		s.input.CursorEnd()
		s.composing = true
		return s.input.Focus()
	case keys.Is(k, keymap.ListBack):
		if s.running() {
			a.confirm("stop", []string{
				"Leaving stops the command in every worktree where it is still running.",
			}, true, "stop and leave", func() tea.Cmd {
				s.stop()
				s.leave(a)
				return nil
			})
			return nil
		}
		s.leave(a)
	}
	return nil
}

// leave pops the screen. Output from processes still winding down is drained
// so their goroutines can finish.
func (s *runScreen) leave(a *App) {
	if ch := s.events; ch != nil {
		go func() {
			for range ch {
			}
		}()
	}
	a.pop()
}

// --- view --------------------------------------------------------------------------

func (s *runScreen) view(a *App, w, h int) string {
	if s.composing {
		return s.composeView(a, w, h)
	}
	if s.zoom {
		return s.outputPanel(w, h)
	}
	// Full branch names are long; give the list two fifths of the width.
	leftW := min(max(w*2/5, 30), 56)
	left := s.jobsPanel(a, leftW, h)
	right := s.outputPanel(w-leftW-1, h)
	sep := make([]string, h)
	for i := range sep {
		sep[i] = " "
		if i < h-1 {
			sep[i] = stMuted.Render("│")
		}
	}
	return joinHorizontal(joinHorizontal(left, strings.Join(sep, "\n")), right)
}

func (s *runScreen) composeView(a *App, w, h int) string {
	inner := w - 4
	s.input.SetWidth(inner - labelW - 1)
	all, branches := s.scopeCounts(a)
	opt := func(on bool, text string) string {
		if on {
			return text
		}
		return stMuted.Render(text)
	}
	body := []string{
		field("command", true, s.input.View()),
		field("in", false, opt(s.scope == scopeAll, fmt.Sprintf("every worktree (%d)", all))+stMuted.Render("   ")+
			opt(s.scope == scopeBranches, fmt.Sprintf("main and checked-out branches (%d)", branches))),
		field("parallel", false, stMuted.Render(fmt.Sprintf("%d at a time", a.cfg.Int(config.KeyParallelism)))),
		"",
	}
	body = append(body, note("Runs with bash -c in each worktree, with $DV_BRANCH and $DV_WORKTREE set.", inner, stMuted)...)
	if len(s.history) > 0 {
		body = append(body, "", field("recent", false, ""))
		room := h - 2 - len(body)
		for i := len(s.history) - 1; i >= 0 && room > 0; i-- {
			line := formIndent + stMuted.Render(s.history[i])
			if i == s.histIdx {
				line = pad(selMarker(), labelW) + rainbow(s.history[i])
			}
			body = append(body, truncate(line, inner))
			room--
		}
	}
	return panel("", body, w, h, true)
}

func (s *runScreen) scopeCounts(a *App) (all, branches int) {
	saved := s.scope
	s.scope = scopeAll
	all = len(s.targets(a))
	s.scope = scopeBranches
	branches = len(s.targets(a))
	s.scope = saved
	return all, branches
}

func (s *runScreen) jobsPanel(a *App, w, h int) string {
	inner := w - 4
	var body []string
	vis := s.visibleJobs()
	listH := h - 2
	start := 0
	for i, idx := range vis {
		if idx == s.cursor && i >= listH {
			start = i - listH + 1
		}
	}
	for _, idx := range vis[start:] {
		if len(body) >= listH {
			break
		}
		j := s.jobs[idx]
		bar := "  "
		if idx == s.cursor {
			bar = selMarker() + " "
		}
		label := j.label
		if idx == s.cursor {
			label = rainbow(label)
		}
		dur := ""
		if !j.start.IsZero() {
			dur = stMuted.Render(duration(j.elapsed()))
		}
		body = append(body, bar+s.icon(a, j)+" "+spread(label, dur, inner-4))
	}
	if len(vis) == 0 {
		body = append(body, stMuted.Render("no failures"))
	}
	title := "worktrees"
	if s.failed {
		title = "failed"
	}
	return panel(title, body, w, h, false)
}

func (s *runScreen) icon(a *App, j *job) string {
	switch j.state {
	case jobQueued:
		return stMuted.Render("·")
	case jobRunning:
		return a.spinner.View()
	case jobOK:
		return stMuted.Render("✓")
	case jobStopped:
		return stWarn.Render("■")
	default:
		return stErr.Render("×")
	}
}

func (s *runScreen) outputPanel(w, h int) string {
	inner := w - 4
	if s.cursor >= len(s.jobs) {
		return panel("", nil, w, h, true)
	}
	j := s.jobs[s.cursor]
	var status string
	switch j.state {
	case jobQueued:
		status = stMuted.Render("queued")
	case jobRunning:
		status = plain.Render("running")
	case jobOK:
		status = plain.Render("exit 0")
	case jobStopped:
		status = stWarn.Render("stopped")
	default:
		if j.errMsg != "" {
			status = stErr.Render(j.errMsg)
		} else {
			status = stErr.Render(fmt.Sprintf("exit %d", j.exit))
		}
	}
	head := spread(stMuted.Render("$ ")+s.command, status, inner)
	viewH := h - 4
	lines := s.tail(j, inner, viewH)
	body := append([]string{head, rule(inner)}, lines...)
	return panel(j.label, body, w, h, true)
}

// tail wraps the end of a job's output to width and returns the viewH lines
// ending s.scroll lines above the bottom. Only as much output as needed is
// wrapped, so huge outputs stay cheap to render.
func (s *runScreen) tail(j *job, w, viewH int) []string {
	src := j.out.all()
	if len(src) == 0 {
		if j.state == jobQueued {
			return []string{stMuted.Render("waiting for a free job slot…")}
		}
		if j.done() {
			return []string{stMuted.Render("(no output)")}
		}
		return nil
	}
	need := viewH + s.scroll
	var wrapped []string
	for i := len(src) - 1; i >= 0 && len(wrapped) < need; i-- {
		parts := strings.Split(ansi.Hardwrap(src[i], w, true), "\n")
		for k := len(parts) - 1; k >= 0; k-- {
			wrapped = append(wrapped, parts[k]+"\x1b[0m")
		}
	}
	// wrapped is reversed; clamp scroll to what exists.
	maxScroll := max(len(wrapped)-viewH, 0)
	if s.scroll > maxScroll {
		s.scroll = maxScroll
	}
	end := s.scroll
	begin := min(end+viewH, len(wrapped))
	out := make([]string, 0, begin-end)
	for i := begin - 1; i >= end; i-- {
		out = append(out, wrapped[i])
	}
	return out
}

func (s *runScreen) status(a *App) string {
	if s.composing || len(s.jobs) == 0 {
		return ""
	}
	done, failed, running := 0, 0, 0
	for _, j := range s.jobs {
		switch j.state {
		case jobOK:
			done++
		case jobFailed, jobStopped:
			done++
			failed++
		case jobRunning:
			running++
		}
	}
	parts := []string{fmt.Sprintf("%d/%d done", done, len(s.jobs))}
	if failed > 0 {
		parts = append(parts, stErr.Render(fmt.Sprintf("%d failed", failed)))
	}
	if running > 0 {
		parts = append(parts, fmt.Sprintf("%d running", running))
	}
	if s.scroll > 0 {
		parts = append(parts, stMuted.Render("scrolled — G to follow"))
	}
	return strings.Join(parts, stMuted.Render(" · "))
}

func (s *runScreen) hints(a *App) []hint {
	if s.composing {
		return []hint{{keys.Key(keymap.TextSubmit), "run"}, {keys.Key(keymap.RunWhere), "where"}, {keys.Pair(keymap.RunHistoryPrev, keymap.RunHistoryNext), "history"}, {keys.Key(keymap.TextCancel), "back"}}
	}
	worktree := hint{keys.Pair(keymap.ListUp, keymap.ListDown), "worktree"}
	scroll := hint{keys.Pair(keymap.RunPageUp, keymap.RunPageDown), "scroll"}
	zoom := hint{keys.Key(keymap.RunZoom), "zoom"}
	failedOnly := hint{keys.Key(keymap.RunFailedOnly), "failed only"}
	if s.running() {
		return []hint{worktree, scroll, zoom, {keys.Key(keymap.RunStop), "stop"}, failedOnly, {keys.Key(keymap.ListHelp), "keys"}}
	}
	return []hint{worktree, scroll, zoom, {keys.Key(keymap.RunRerunFailed), "rerun failed"}, {keys.Key(keymap.RunEdit), "edit"}, failedOnly, {keys.Key(keymap.ListBack), "back"}}
}

func (s *runScreen) help() []helpSection {
	return []helpSection{{"Compose", []hint{
		{keys.Keys(keymap.TextSubmit), "run the command"},
		{keys.Keys(keymap.RunWhere), "every worktree, or only main and checked-out branches"},
		{keys.Keys(keymap.RunHistoryPrev) + " " + keys.Keys(keymap.RunHistoryNext), "command history"},
		{keys.Keys(keymap.TextCancel), "back"},
	}}, {"Output", []hint{
		{keys.Keys(keymap.ListUp) + " / " + keys.Keys(keymap.ListDown), "choose a worktree"},
		{keys.Keys(keymap.RunPageUp) + " / " + keys.Keys(keymap.RunPageDown), "scroll output"},
		{keys.Keys(keymap.RunTop) + " / " + keys.Keys(keymap.RunBottom), "top / bottom (bottom follows new output)"},
		{keys.Keys(keymap.RunZoom), "zoom the output"},
		{keys.Keys(keymap.RunFailedOnly), "show failures only"},
		{keys.Keys(keymap.RunRerunFailed), "rerun where it failed"},
		{keys.Keys(keymap.RunRerunAll), "rerun everywhere"},
		{keys.Keys(keymap.RunEdit), "edit the command"},
		{keys.Keys(keymap.RunStop) + " ctrl+c", "stop"},
		{keys.Keys(keymap.ListBack), "back"},
	}}}
}

// joinHorizontal places two equally tall blocks side by side.
func joinHorizontal(left, right string) string {
	l := strings.Split(left, "\n")
	r := strings.Split(right, "\n")
	out := make([]string, max(len(l), len(r)))
	for i := range out {
		var a, b string
		if i < len(l) {
			a = l[i]
		}
		if i < len(r) {
			b = r[i]
		}
		out[i] = a + b
	}
	return strings.Join(out, "\n")
}

// --- history -----------------------------------------------------------------------

const maxHistory = 100

func historyPath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "dv", "run_history")
}

func loadHistory() []string {
	p := historyPath()
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// saveHistory appends command, moving an earlier copy to the end.
func saveHistory(hist []string, command string) {
	p := historyPath()
	if p == "" {
		return
	}
	var out []string
	for _, h := range hist {
		if h != command {
			out = append(out, h)
		}
	}
	out = append(out, command)
	if len(out) > maxHistory {
		out = out[len(out)-maxHistory:]
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(p, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

func (s *runScreen) title() string { return "run" }
