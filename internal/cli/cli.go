// Package cli implements dv's command line: the TUI entry point, subcommands
// for scripting, and the handoff that lets the shell change directory.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"github.com/talmage89/dv/internal/config"
	"github.com/talmage89/dv/internal/pool"
	"github.com/talmage89/dv/internal/tui"
)

// exitLegacyPassthrough tells the pre-2.0 shell wrapper "not a cd target, run
// me normally". Already-open shells still have that wrapper loaded.
const exitLegacyPassthrough = 99

// removed maps retired subcommands to why they are gone.
var removed = map[string]string{
	"tidy":     "worktrees are an anonymous pool now, so there is nothing to repack",
	"init":     "use `dv scale <n>`",
	"add":      "use `dv scale <n>`",
	"user":     "use `dv config user [<name>|--unset]`",
	"repo":     "use `dv config repo [<path>|--unset]`",
	"branches": "run `dv` for the interface, or `dv ls` for a plain list",
	"migrate":  "the old alias setup is long gone",
	"tutorial": "run `dv` and press ? for keys",
}

// session carries per-invocation state.
type session struct {
	ctx     context.Context
	version string
	legacy  bool // invoked through the old `__resolve_path` wrapper
}

// Main runs dv with args and returns the exit code.
func Main(ctx context.Context, args []string, version string) int {
	s := &session{ctx: ctx, version: version}
	if len(args) > 0 && args[0] == "__resolve_path" {
		s.legacy = true
		return s.resolvePath(args[1:])
	}
	return s.dispatch(args)
}

func (s *session) dispatch(args []string) int {
	if len(args) == 0 {
		return s.runTUI()
	}
	sub, rest := args[0], args[1:]
	if why, ok := removed[sub]; ok {
		fmt.Fprintf(os.Stderr, "dv %s was removed: %s.\n", sub, why)
		return 2
	}
	switch sub {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	case "-v", "--version", "version":
		fmt.Println("dv", s.version)
		return 0
	case "shell-init":
		fmt.Print(shellInit)
		return 0
	case "install":
		return install(rest)
	case "config":
		return s.config(rest)
	case "ls":
		if len(rest) > 0 {
			fmt.Fprintln(os.Stderr, "usage: dv ls")
			return 2
		}
		return s.withRepo(s.ls)
	case "new":
		return s.withRepo(func(r *pool.Repo) int { return s.newBranch(r, rest) })
	case "main":
		return s.withRepo(func(r *pool.Repo) int { return s.cd(r.Main) })
	case "run":
		return s.withRepo(func(r *pool.Repo) int { return s.run(r, rest) })
	case "sync":
		return s.withRepo(func(r *pool.Repo) int { return s.sync(r, rest) })
	case "unsync":
		return s.withRepo(func(r *pool.Repo) int { return s.unsync(r, rest) })
	case "prune":
		return s.withRepo(func(r *pool.Repo) int { return s.prune(r, rest) })
	case "sweep":
		return s.withRepo(func(r *pool.Repo) int { return s.sweep(r, rest) })
	case "park", "unpark":
		return s.withRepo(func(r *pool.Repo) int { return s.park(r, sub, rest) })
	case "scale":
		return s.withRepo(func(r *pool.Repo) int { return s.scale(r, rest) })
	}
	if _, err := strconv.Atoi(sub); err == nil {
		fmt.Fprintln(os.Stderr, "dv <N> was removed: worktrees no longer have meaningful numbers. Use `dv <branch>` or the interface.")
		return 2
	}
	if strings.HasPrefix(sub, "-") {
		fmt.Fprintf(os.Stderr, "dv: unknown flag %s\n\n%s", sub, usage)
		return 2
	}
	return s.withRepo(func(r *pool.Repo) int { return s.checkout(r, sub) })
}

const usage = `dv — a pool of git worktrees

  dv                     open the interface
  dv <branch>            go to <branch>, checking it out into a free worktree if needed
  dv new <branch>        create <branch> from the base ref in a free worktree and go there
  dv main                go to the main worktree

  dv ls                  list checked-out and parked branches, most recent first
  dv park <branch>       free <branch>'s worktree but keep it listed; dv unpark undoes
  dv prune [--force]     delete branches whose upstream was deleted, freeing worktrees
  dv sweep               free worktrees holding branches with no commit of yours
  dv scale [<n>]         show or change the number of pooled worktrees
  dv run [-x <dir>]... <command>
                         run a command in every worktree, one after another
  dv sync [env|claude] [--force]
                         sync .env and .claude into every worktree
  dv unsync claude       remove the .claude links

  dv config [<key> [<value> | --unset]]
  dv install [--write]   set up the shell integration (needed for dv to change directory)
  dv shell-init          print the shell function (eval "$(dv shell-init)")
  dv version
`

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "dv:", err)
	return 1
}

// loadRepo resolves config and the target repository.
func (s *session) loadRepo() (*config.Config, *pool.Repo, error) {
	cfg, err := config.Load(s.ctx)
	if err != nil {
		return nil, nil, err
	}
	repo, err := pool.Open(s.ctx, cfg)
	if errors.Is(err, pool.ErrNoRepo) {
		return nil, nil, fmt.Errorf("%w.\n\n"+
			"  dv                      run the interface to be asked for one\n"+
			"  dv config repo <path>   remember one (any worktree of it)\n"+
			"  DEV_REPO=<path>         or set it per shell\n"+
			"  or run dv from inside the repo", pool.ErrNoRepo)
	}
	return cfg, repo, err
}

func (s *session) withRepo(fn func(*pool.Repo) int) int {
	_, repo, err := s.loadRepo()
	if err != nil {
		return fail(err)
	}
	return fn(repo)
}

// cd hands a directory to the shell integration.
func (s *session) cd(path string) int {
	if f := os.Getenv("DV_CD_FILE"); f != "" {
		if err := os.WriteFile(f, []byte(path), 0o600); err != nil {
			return fail(err)
		}
		return 0
	}
	if s.legacy {
		fmt.Println(path)
		return 0
	}
	fmt.Fprintf(os.Stderr, "dv: shell integration not installed (see `dv install`), so run:\n  cd %s\n", path)
	return 0
}

func canCD(legacy bool) bool { return legacy || os.Getenv("DV_CD_FILE") != "" }

func (s *session) runTUI() int {
	var opts []tea.ProgramOption
	var out io.Writer = os.Stdout
	// Under the legacy wrapper stdout is captured, so draw on the terminal
	// directly and keep stdout for the cd target.
	if !term.IsTerminal(os.Stdout.Fd()) || !term.IsTerminal(os.Stdin.Fd()) {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return fail(errors.New("the interface needs a terminal"))
		}
		defer tty.Close()
		opts = append(opts, tea.WithInput(tty), tea.WithOutput(tty))
		out = tty
	}
	cfg, repo, err := s.loadRepo()
	if errors.Is(err, pool.ErrNoRepo) {
		saved, setupErr := tui.RunSetup(s.ctx, opts...)
		if setupErr != nil {
			return fail(setupErr)
		}
		if !saved {
			return 0
		}
		cfg, repo, err = s.loadRepo()
	}
	if err != nil {
		return fail(err)
	}
	res, err := tui.Run(s.ctx, cfg, repo, tui.Options{Version: s.version, CanCD: canCD(s.legacy)}, opts...)
	// Zellij keeps one kitty keyboard flag per screen rather than a stack, so
	// the TUI's flag can survive onto the main screen and the shell then gets
	// Ctrl+L as ESC[108;5u. Reset both keyboard protocols back on the shell's screen.
	_, _ = io.WriteString(out, ansi.ResetModifyOtherKeys+ansi.PopKittyKeyboard(1))
	if err != nil {
		return fail(err)
	}
	if res.Note != "" {
		fmt.Fprintln(os.Stderr, res.Note)
	}
	if res.CD != "" {
		return s.cd(res.CD)
	}
	return 0
}

// resolvePath implements the protocol of the pre-2.0 wrapper: print a
// directory and exit 0 to cd, or exit 99 to be re-run without capture.
func (s *session) resolvePath(args []string) int {
	if len(args) == 0 {
		return s.runTUI()
	}
	switch args[0] {
	case "main", "new":
		return s.dispatch(args)
	}
	if _, err := strconv.Atoi(args[0]); err == nil {
		return s.dispatch(args)
	}
	if isSubcommand(args[0]) {
		return exitLegacyPassthrough
	}
	return s.dispatch(args)
}

func isSubcommand(name string) bool {
	if _, ok := removed[name]; ok {
		return true
	}
	switch name {
	case "-h", "--help", "help", "-v", "--version", "version", "shell-init", "install", "config",
		"ls", "run", "sync", "unsync", "prune", "sweep", "park", "unpark", "scale":
		return true
	}
	return strings.HasPrefix(name, "-")
}

// --- subcommands -------------------------------------------------------------------

func (s *session) checkout(r *pool.Repo, branch string) int {
	res, err := r.Checkout(s.ctx, branch)
	if err != nil {
		return fail(err)
	}
	if res.Moved {
		fmt.Fprintf(os.Stderr, "Checked out %s\n", branch)
	}
	return s.cd(res.Path)
}

func (s *session) newBranch(r *pool.Repo, args []string) int {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	base := fs.String("base", "", "ref to start from (default: the configured base)")
	if err := fs.Parse(reorder(args)); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: dv new [--base <ref>] <branch>")
		return 2
	}
	path, err := r.NewBranch(s.ctx, fs.Arg(0), *base)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "Created %s\n", fs.Arg(0))
	return s.cd(path)
}

// reorder moves flags before positional arguments so `dv new foo --base x`
// works with the standard flag package.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) && args[i] != "--force" && args[i] != "-f" && args[i] != "--no-fetch" && args[i] != "--write" {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, args[i])
	}
	return append(flags, pos...)
}

func (s *session) ls(r *pool.Repo) int {
	snap, err := r.Snapshot(s.ctx)
	if err != nil {
		return fail(err)
	}
	var paths []string
	for _, b := range snap.CheckedOut() {
		paths = append(paths, b.Worktree)
	}
	statuses := map[string]pool.Status{}
	var mu sync.Mutex
	pool.StatusAll(s.ctx, paths, func(p string, st pool.Status) {
		mu.Lock()
		statuses[p] = st
		mu.Unlock()
	})
	color := term.IsTerminal(os.Stdout.Fd())
	paint := func(code, text string) string {
		if !color || text == "" {
			return text
		}
		return "\x1b[" + code + "m" + text + "\x1b[0m"
	}
	printRow := func(b *pool.Branch, marker string) {
		fmt.Printf("%s %-4s %s\n", marker, ago(b.LastActive), b.Name)
	}
	for _, b := range snap.CheckedOut() {
		marker := " "
		if st := statuses[b.Worktree]; st.Dirty {
			marker = paint("31", "*")
		} else if st.Err != "" {
			marker = paint("33", "?")
		}
		printRow(b, marker)
	}
	if parked := snap.Parked(); len(parked) > 0 {
		fmt.Println(paint("2", "parked"))
		for _, b := range parked {
			printRow(b, " ")
		}
	}
	return 0
}

func (s *session) run(r *pool.Repo, args []string) int {
	exclude := map[string]bool{}
	for len(args) >= 2 && (args[0] == "-x" || args[0] == "--exclude") {
		exclude[args[1]] = true
		args = args[2:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: dv run [-x <dir>]... <command> [args...]")
		return 2
	}
	snap, err := r.Snapshot(s.ctx)
	if err != nil {
		return fail(err)
	}
	failed := 0
	targets := append([]pool.Slot{snap.Main}, snap.Slots...)
	for _, sl := range targets {
		if exclude[sl.Name] || sl.Missing {
			continue
		}
		cmd := exec.CommandContext(s.ctx, args[0], args[1:]...)
		cmd.Dir = sl.Path
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = append(os.Environ(), "DV_WORKTREE="+sl.Path, "DV_BRANCH="+sl.Branch,
			"DV_MAIN="+r.Main, "DV_ROOT="+r.Root)
		if err := cmd.Run(); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "dv: %s: %v\n", sl.Name, err)
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func (s *session) syncTargets(r *pool.Repo) ([]string, error) {
	snap, err := r.Snapshot(s.ctx)
	if err != nil {
		return nil, err
	}
	paths := []string{snap.Main.Path}
	for _, sl := range snap.Slots {
		if !sl.Missing {
			paths = append(paths, sl.Path)
		}
	}
	return paths, nil
}

func (s *session) sync(r *pool.Repo, args []string) int {
	mode := pool.SyncAuto
	what := ""
	for _, a := range args {
		switch a {
		case "--force", "-f":
			mode = pool.SyncForce
		case "env", "claude":
			what = a
		default:
			fmt.Fprintln(os.Stderr, "usage: dv sync [env|claude] [--force]")
			return 2
		}
	}
	paths, err := s.syncTargets(r)
	if err != nil {
		return fail(err)
	}
	var rep pool.SyncReport
	switch what {
	case "env":
		rep = r.SyncEnv(paths, mode)
	case "claude":
		rep = r.SyncClaude(s.ctx, paths, mode)
	default:
		rep.Merge(r.SyncEnv(paths, mode))
		rep.Merge(r.SyncClaude(s.ctx, paths, mode))
	}
	for _, c := range rep.Changes {
		fmt.Println(c)
	}
	for _, i := range rep.Issues {
		fmt.Fprintln(os.Stderr, "warning:", i)
	}
	if len(rep.Changes) == 0 && len(rep.Issues) == 0 {
		fmt.Println("Everything is in sync.")
	}
	if len(rep.Issues) > 0 {
		return 1
	}
	return 0
}

func (s *session) unsync(r *pool.Repo, args []string) int {
	if len(args) != 1 || args[0] != "claude" {
		fmt.Fprintln(os.Stderr, "usage: dv unsync claude")
		return 2
	}
	paths, err := s.syncTargets(r)
	if err != nil {
		return fail(err)
	}
	for _, p := range paths[1:] {
		n, err := removeLinks(p + "/.claude")
		if err != nil {
			fmt.Fprintf(os.Stderr, "dv: %s: %v\n", p, err)
			continue
		}
		if n > 0 {
			fmt.Printf("%s: removed %d link(s)\n", p, n)
		}
	}
	return 0
}

func (s *session) prune(r *pool.Repo, args []string) int {
	force, fetch := false, true
	for _, a := range args {
		switch a {
		case "--force", "-f":
			force = true
		case "--no-fetch":
			fetch = false
		default:
			fmt.Fprintln(os.Stderr, "usage: dv prune [--force] [--no-fetch]")
			return 2
		}
	}
	if fetch {
		fmt.Println("Fetching origin…")
		if err := r.Fetch(s.ctx); err != nil {
			fmt.Fprintln(os.Stderr, "dv:", err, "(using the last fetch)")
		}
	}
	cands, err := r.PruneCandidates(s.ctx)
	if err != nil {
		return fail(err)
	}
	if len(cands) == 0 {
		fmt.Println("Nothing to prune.")
		return 0
	}
	code := 0
	for _, c := range cands {
		switch {
		case c.InMain:
			fmt.Printf("skip    %s (checked out in the main worktree)\n", c.Branch)
			continue
		case c.Parked:
			fmt.Printf("skip    %s (parked; unpark it or prune from the interface)\n", c.Branch)
			continue
		case c.Status.Dirty && !force:
			fmt.Printf("skip    %s (uncommitted changes; --force discards them)\n", c.Branch)
			continue
		}
		if err := r.Prune(s.ctx, c, force && c.Status.Dirty); err != nil {
			fmt.Fprintf(os.Stderr, "failed  %s: %v\n", c.Branch, err)
			code = 1
			continue
		}
		fmt.Printf("pruned  %s\n", c.Branch)
	}
	return code
}

func (s *session) sweep(r *pool.Repo, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: dv sweep")
		return 2
	}
	cands, err := r.SweepCandidates(s.ctx)
	if err != nil {
		return fail(err)
	}
	if len(cands) == 0 {
		fmt.Println("Nothing to sweep.")
		return 0
	}
	code := 0
	for _, c := range cands {
		switch {
		case c.Status.Err != "":
			fmt.Printf("skip    %s (status unreadable: %s)\n", c.Branch, c.Status.Err)
			continue
		case c.Status.Dirty:
			fmt.Printf("skip    %s (uncommitted changes)\n", c.Branch)
			continue
		case c.Live > 0:
			fmt.Printf("skip    %s (Claude is running there; sweep it from the interface)\n", c.Branch)
			continue
		}
		if err := r.Release(s.ctx, c.Branch); err != nil {
			fmt.Fprintf(os.Stderr, "failed  %s: %v\n", c.Branch, err)
			code = 1
			continue
		}
		fmt.Printf("freed   %s (%s)\n", c.Branch, filepath.Base(c.Worktree))
	}
	return code
}

func (s *session) park(r *pool.Repo, sub string, args []string) int {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: dv %s <branch>\n", sub)
		return 2
	}
	var err error
	if sub == "park" {
		err = r.Park(s.ctx, args[0])
	} else {
		err = r.Unpark(s.ctx, args[0])
	}
	if err != nil {
		return fail(err)
	}
	fmt.Printf("%sed %s\n", sub, args[0])
	return 0
}

func (s *session) scale(r *pool.Repo, args []string) int {
	snap, err := r.Snapshot(s.ctx)
	if err != nil {
		return fail(err)
	}
	if len(args) == 0 {
		fmt.Printf("%d pooled worktrees in %s\n", len(snap.Slots), r.Root)
		return 0
	}
	target, err := strconv.Atoi(args[0])
	if err != nil || target < 0 {
		fmt.Fprintln(os.Stderr, "usage: dv scale [<n>]")
		return 2
	}
	var paths []string
	for _, sl := range snap.Slots {
		paths = append(paths, sl.Path)
	}
	statuses := map[string]pool.Status{}
	var mu sync.Mutex
	pool.StatusAll(s.ctx, paths, func(p string, st pool.Status) {
		mu.Lock()
		statuses[p] = st
		mu.Unlock()
	})
	plan := r.PlanScale(snap, target, statuses, pool.LiveSessions(s.ctx, paths))
	if len(plan.Add) > 0 {
		if err := r.PrepareAdd(s.ctx); err != nil {
			return fail(err)
		}
	}
	code := 0
	var added []string
	for _, p := range plan.Add {
		if err := r.AddSlot(s.ctx, p); err != nil {
			fmt.Fprintln(os.Stderr, "dv:", err)
			code = 1
			continue
		}
		added = append(added, p)
		fmt.Println("created", p)
	}
	for _, sl := range plan.Remove {
		if err := r.RemoveSlot(s.ctx, sl); err != nil {
			fmt.Fprintln(os.Stderr, "dv:", err)
			code = 1
			continue
		}
		fmt.Println("removed", sl.Path)
	}
	if plan.Shortfall > 0 {
		fmt.Fprintf(os.Stderr, "dv: kept %d worktrees that hold branches, have changes, or run Claude\n", plan.Shortfall)
		code = 1
	}
	if len(added) > 0 {
		rep := r.Sync(s.ctx, added, pool.SyncAuto)
		for _, i := range rep.Issues {
			fmt.Fprintln(os.Stderr, "warning:", i)
		}
	}
	return code
}

func (s *session) config(args []string) int {
	cfg, err := config.Load(s.ctx)
	if err != nil {
		return fail(err)
	}
	if len(args) == 0 {
		// Resolve the repo so derived defaults (root, .env source) show.
		_, _ = pool.Open(s.ctx, cfg)
		w := 0
		for _, sp := range config.Specs {
			w = max(w, len(sp.Key))
		}
		for _, sp := range config.Specs {
			v := cfg.Get(sp.Key)
			fmt.Printf("%-*s  %s  (%s)\n", w, sp.Key, v.Value, v.SourceLabel())
		}
		return 0
	}
	key := config.Key(args[0])
	if _, ok := config.SpecFor(key); !ok {
		keys := make([]string, len(config.Specs))
		for i, sp := range config.Specs {
			keys[i] = string(sp.Key)
		}
		sort.Strings(keys)
		fmt.Fprintf(os.Stderr, "dv: unknown setting %q (known: %s)\n", key, strings.Join(keys, ", "))
		return 2
	}
	switch {
	case len(args) == 1:
		_, _ = pool.Open(s.ctx, cfg)
		v := cfg.Get(key)
		fmt.Printf("%s  (%s)\n", v.Value, v.SourceLabel())
	case args[1] == "--unset":
		if err := config.Unset(s.ctx, key); err != nil {
			return fail(err)
		}
		fmt.Printf("unset dev.%s\n", key)
	default:
		if err := config.Set(s.ctx, key, args[1]); err != nil {
			return fail(err)
		}
		fmt.Printf("set dev.%s\n", key)
		if spec, _ := config.SpecFor(key); spec.EnvVar != "" && os.Getenv(spec.EnvVar) != "" {
			fmt.Fprintf(os.Stderr, "note: $%s is set in your environment and overrides this\n", spec.EnvVar)
		}
	}
	return 0
}
