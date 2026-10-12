package pool

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/talmage89/dv/internal/config"
)

// fixture is a throwaway repository laid out like a real dv setup:
//
//	<tmp>/origin.git     bare remote
//	<tmp>/root/app       main worktree
//	<tmp>/root/dev-1..n  pool slots, detached at origin/main
type fixture struct {
	t    *testing.T
	dir  string
	root string
	main string
	repo *Repo
}

// isolateGit points git at an empty global config so the developer's own
// settings (commit signing, hooks, aliases) cannot leak into tests.
func isolateGit(t *testing.T) string {
	t.Helper()
	// Resolve symlinks (macOS temp dirs live under /var -> /private/var) so
	// paths compare equal to the real paths git reports.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(global, []byte("[user]\n\tname = dv test\n\temail = dv@example.com\n[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	for _, k := range []string{"DEV_REPO", "DEV_ROOT", "DEV_USER"} {
		t.Setenv(k, "")
	}
	return dir
}

func newFixture(t *testing.T, slots int) *fixture {
	t.Helper()
	dir := isolateGit(t)
	f := &fixture{t: t, dir: dir, root: filepath.Join(dir, "root")}
	f.main = filepath.Join(f.root, "app")
	origin := filepath.Join(dir, "origin.git")
	f.git(dir, "init", "--bare", "-q", origin)
	f.git(dir, "clone", "-q", origin, f.main)
	f.write(f.main, "README.md", "hello\n")
	f.write(f.main, ".gitignore", ".env\n.claude/*.local.md\n.claude/rules/\n")
	f.git(f.main, "add", ".")
	f.git(f.main, "commit", "-qm", "init")
	f.git(f.main, "push", "-q", "origin", "main")
	for i := 1; i <= slots; i++ {
		f.git(f.main, "worktree", "add", "-q", "--detach", filepath.Join(f.root, "dev-"+itoa(i)), "origin/main")
	}
	t.Setenv("DEV_REPO", f.main)
	t.Setenv("DEV_ROOT", f.root)
	t.Setenv("DEV_USER", "me")
	f.reopen()
	return f
}

func (f *fixture) reopen() {
	f.t.Helper()
	cfg, err := config.Load(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	repo, err := Open(context.Background(), cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.repo = repo
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(dir, rel, content string) {
	f.t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) slot(n int) string { return filepath.Join(f.root, "dev-"+itoa(n)) }

// branch creates a local branch with one commit, without checking it out.
func (f *fixture) branch(name string) {
	f.t.Helper()
	f.git(f.main, "branch", name, "origin/main")
}

func (f *fixture) snap() *Snapshot {
	f.t.Helper()
	s, err := f.repo.Snapshot(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestSnapshotClassifiesWorktrees(t *testing.T) {
	f := newFixture(t, 3)
	ext := filepath.Join(f.dir, "elsewhere")
	f.git(f.main, "worktree", "add", "-q", "-b", "me/ext", ext, "origin/main")
	s := f.snap()
	if s.Main.Path != f.main || !s.Main.Main {
		t.Fatalf("main = %+v", s.Main)
	}
	if len(s.Slots) != 3 || s.Slots[0].Num != 1 || s.Slots[2].Num != 3 {
		t.Fatalf("slots = %+v", s.Slots)
	}
	if len(s.External) != 1 || s.External[0].Branch != "me/ext" {
		t.Fatalf("external = %+v", s.External)
	}
	if got := s.CheckedOut(); len(got) != 1 || got[0].Name != "me/ext" {
		t.Fatalf("checked out = %v", names(got))
	}
}

func TestSlotNumbersSortNumerically(t *testing.T) {
	f := newFixture(t, 0)
	for _, n := range []int{10, 2, 1} {
		f.git(f.main, "worktree", "add", "-q", "--detach", f.slot(n), "origin/main")
	}
	s := f.snap()
	var got []int
	for _, sl := range s.Slots {
		got = append(got, sl.Num)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 10 {
		t.Fatalf("order = %v", got)
	}
}

func TestCheckoutUsesLowestCleanSlot(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	f.write(f.slot(1), "scratch.txt", "dirty\n")
	f.branch("me/a")
	res, err := f.repo.Checkout(ctx, "me/a")
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != f.slot(2) || !res.Moved {
		t.Fatalf("checkout = %+v, want dev-2 (dev-1 is dirty)", res)
	}
	again, err := f.repo.Checkout(ctx, "me/a")
	if err != nil || again.Path != f.slot(2) || again.Moved {
		t.Fatalf("second checkout = %+v, %v", again, err)
	}
}

func TestCheckoutRemoteOnlyBranchTracksOrigin(t *testing.T) {
	f := newFixture(t, 1)
	f.git(f.main, "push", "-q", "origin", "origin/main:refs/heads/teammate/x")
	res, err := f.repo.Checkout(context.Background(), "teammate/x")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Tracked {
		t.Fatal("expected a tracking branch to be created")
	}
	if up := f.git(res.Path, "rev-parse", "--abbrev-ref", "@{upstream}"); up != "origin/teammate/x" {
		t.Fatalf("upstream = %q", up)
	}
}

func TestCheckoutErrors(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	if _, err := f.repo.Checkout(ctx, "nope"); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("err = %v, want ErrBranchNotFound", err)
	}
	f.branch("me/a")
	f.branch("me/b")
	if _, err := f.repo.Checkout(ctx, "me/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Checkout(ctx, "me/b"); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("err = %v, want ErrNoFreeSlot", err)
	}
}

func TestNewBranchStartsFromBaseWithoutUpstream(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	path, err := f.repo.NewBranch(ctx, "me/new", "")
	if err != nil {
		t.Fatal(err)
	}
	if path != f.slot(1) {
		t.Fatalf("path = %s", path)
	}
	if got := f.git(path, "branch", "--show-current"); got != "me/new" {
		t.Fatalf("branch = %q", got)
	}
	if out, err := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "@{upstream}").CombinedOutput(); err == nil {
		t.Fatalf("unexpected upstream %s", out)
	}
	if _, err := f.repo.NewBranch(ctx, "me/new", ""); !errors.Is(err, ErrBranchExists) {
		t.Fatalf("err = %v, want ErrBranchExists", err)
	}
	if _, err := f.repo.NewBranch(ctx, "bad..name", ""); err == nil {
		t.Fatal("expected invalid name error")
	}
}

func TestParkRefusesDirtyAndFreesSlot(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	f.branch("me/spike")
	res, err := f.repo.Checkout(ctx, "me/spike")
	if err != nil {
		t.Fatal(err)
	}
	f.write(res.Path, "test.md", "")
	if err := f.repo.Park(ctx, "me/spike"); !errors.Is(err, ErrDirty) {
		t.Fatalf("err = %v, want ErrDirty", err)
	}
	os.Remove(filepath.Join(res.Path, "test.md"))
	if err := f.repo.Park(ctx, "me/spike"); err != nil {
		t.Fatal(err)
	}
	s := f.snap()
	if !s.Slots[0].Detached() {
		t.Fatal("slot still holds the branch")
	}
	if got := s.Parked(); len(got) != 1 || got[0].Name != "me/spike" {
		t.Fatalf("parked = %v", names(got))
	}
	// Checking a parked branch out again unparks it.
	if _, err := f.repo.Checkout(ctx, "me/spike"); err != nil {
		t.Fatal(err)
	}
	if b := f.snap().Branches["me/spike"]; b.Parked {
		t.Fatal("still parked after checkout")
	}
}

func TestParkedMarkDisappearsWithBranch(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	f.branch("me/old")
	if err := f.repo.Park(ctx, "me/old"); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Delete(ctx, "me/old"); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", f.main, "config", "--get", "branch.me/old.dvparked").CombinedOutput(); err == nil {
		t.Fatalf("parked mark survived deletion: %s", out)
	}
}

func TestDeleteFreesSlot(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	f.branch("me/done")
	if _, err := f.repo.Checkout(ctx, "me/done"); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Delete(ctx, "me/done"); err != nil {
		t.Fatal(err)
	}
	s := f.snap()
	if s.Branches["me/done"] != nil || !s.Slots[0].Detached() {
		t.Fatal("branch or slot not cleaned up")
	}
}

func TestDeleteRefusesMainWorktree(t *testing.T) {
	f := newFixture(t, 1)
	if err := f.repo.Delete(context.Background(), "main"); !errors.Is(err, ErrInMain) {
		t.Fatalf("err = %v, want ErrInMain", err)
	}
}

func TestPruneGoneBranches(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	for _, b := range []string{"me/merged", "me/dirty"} {
		if _, err := f.repo.NewBranch(ctx, b, ""); err != nil {
			t.Fatal(err)
		}
		slot, _ := f.snap().SlotFor(b)
		f.git(slot.Path, "push", "-q", "-u", "origin", b)
		f.git(f.main, "push", "-q", "origin", "--delete", b)
	}
	dirtySlot, _ := f.snap().SlotFor("me/dirty")
	f.write(dirtySlot.Path, "wip.txt", "x")
	if err := f.repo.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	cands, err := f.repo.PruneCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]PruneCandidate{}
	for _, c := range cands {
		byName[c.Branch] = c
	}
	if c := byName["me/merged"]; !c.DefaultSelected() {
		t.Fatalf("clean gone branch should be selected: %+v", c)
	}
	if c := byName["me/dirty"]; c.DefaultSelected() || !c.Status.Dirty {
		t.Fatalf("dirty gone branch should not be selected: %+v", c)
	}
	if err := f.repo.Prune(ctx, byName["me/merged"], false); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Prune(ctx, byName["me/dirty"], false); !errors.Is(err, ErrDirty) {
		t.Fatalf("err = %v, want ErrDirty", err)
	}
	if err := f.repo.Prune(ctx, byName["me/dirty"], true); err != nil {
		t.Fatal(err)
	}
	if n := len(f.snap().Gone()); n != 0 {
		t.Fatalf("%d gone branches remain", n)
	}
}

func TestPlanScale(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	// Leave a gap at dev-2 and make dev-3 hold a branch.
	f.git(f.main, "worktree", "remove", f.slot(2))
	f.branch("me/a")
	if _, err := f.repo.Checkout(ctx, "me/a"); err != nil { // lands in dev-1
		t.Fatal(err)
	}
	s := f.snap()
	statuses := map[string]Status{}
	for _, sl := range s.Slots {
		statuses[sl.Path] = StatusOf(ctx, sl.Path)
	}
	up := f.repo.PlanScale(s, 4, statuses, nil)
	if len(up.Add) != 2 || up.Add[0] != f.slot(2) || up.Add[1] != f.slot(4) {
		t.Fatalf("add = %v", up.Add)
	}
	down := f.repo.PlanScale(s, 0, statuses, nil)
	if len(down.Remove) != 1 || down.Remove[0].Path != f.slot(3) || down.Shortfall != 1 {
		t.Fatalf("remove = %+v shortfall %d", down.Remove, down.Shortfall)
	}
	live := map[string]int{f.slot(3): 1}
	if p := f.repo.PlanScale(s, 1, statuses, live); len(p.Remove) != 0 {
		t.Fatalf("removed a slot with a live session: %+v", p.Remove)
	}
	for _, p := range up.Add {
		if err := f.repo.AddSlot(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.repo.RemoveSlot(ctx, down.Remove[0]); err != nil {
		t.Fatal(err)
	}
	if n := len(f.snap().Slots); n != 3 {
		t.Fatalf("slots = %d, want 3", n)
	}
}

func TestSyncEnv(t *testing.T) {
	f := newFixture(t, 2)
	if err := config.Set(context.Background(), config.KeyEnvLine, "DATABASE_URL=postgres://localhost/app-{name}"); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	src := filepath.Join(f.root, ".env")
	f.write(f.root, ".env", "A=1")
	past := time.Now().Add(-time.Hour)
	os.Chtimes(src, past, past)
	wts := []string{f.main, f.slot(1), f.slot(2)}

	rep := f.repo.SyncEnv(wts, SyncAuto)
	if len(rep.Issues) != 0 || len(rep.Changes) != 3 {
		t.Fatalf("first sync = %+v", rep)
	}
	got, _ := os.ReadFile(filepath.Join(f.slot(1), ".env"))
	want := "A=1\nDATABASE_URL=postgres://localhost/app-dev-1\n"
	if string(got) != want {
		t.Fatalf(".env = %q, want %q", got, want)
	}
	if rep := f.repo.SyncEnv(wts, SyncAuto); len(rep.Changes) != 0 {
		t.Fatalf("second sync changed %v", rep.Changes)
	}

	// A local edit newer than the source is reported, not overwritten.
	f.write(f.slot(2), ".env", "LOCAL=1\n")
	rep = f.repo.SyncEnv(wts, SyncAuto)
	if len(rep.Issues) != 1 || rep.Issues[0].Worktree != "dev-2" {
		t.Fatalf("issues = %+v", rep.Issues)
	}
	if rep := f.repo.SyncEnv(wts, SyncForce); len(rep.Changes) != 1 {
		t.Fatalf("force changes = %v", rep.Changes)
	}

	// A source edited after the worktree copies propagates automatically.
	f.write(f.root, ".env", "A=2\n")
	if rep := f.repo.SyncEnv(wts, SyncAuto); len(rep.Changes) != 3 || len(rep.Issues) != 0 {
		t.Fatalf("propagate = %+v", rep)
	}
}

func TestSyncEnvMissingSource(t *testing.T) {
	f := newFixture(t, 1)
	rep := f.repo.SyncEnv([]string{f.slot(1)}, SyncAuto)
	if len(rep.Issues) != 1 || rep.Issues[0].Severity != SeverityWarn {
		t.Fatalf("issues = %+v", rep.Issues)
	}
}

func TestSyncClaude(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	f.write(f.main, ".claude/CLAUDE.local.md", "local")
	f.write(f.main, ".claude/rules/react.md", "rules")
	f.write(f.main, ".claude/settings.json", "tracked") // not ignored
	wts := []string{f.main, f.slot(1), f.slot(2)}

	rep := f.repo.SyncClaude(ctx, wts, SyncAuto)
	if len(rep.Issues) != 0 || len(rep.Changes) != 4 {
		t.Fatalf("first sync = %+v", rep)
	}
	link := filepath.Join(f.slot(1), ".claude", "rules", "react.md")
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(f.main, ".claude", "rules", "react.md") {
		t.Fatalf("link = %q, %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(f.slot(1), ".claude", "settings.json")); err == nil {
		t.Fatal("tracked file was linked")
	}

	// A regular file where a link belongs is reported in auto mode.
	conflict := filepath.Join(f.slot(2), ".claude", "CLAUDE.local.md")
	os.Remove(conflict)
	f.write(f.slot(2), ".claude/CLAUDE.local.md", "mine")
	rep = f.repo.SyncClaude(ctx, wts, SyncAuto)
	if len(rep.Issues) != 1 {
		t.Fatalf("issues = %+v", rep.Issues)
	}
	if rep := f.repo.SyncClaude(ctx, wts, SyncForce); len(rep.Changes) != 1 {
		t.Fatalf("force = %+v", rep)
	}

	// Removing the source removes links and the directories they emptied.
	os.RemoveAll(filepath.Join(f.main, ".claude", "rules"))
	f.repo.SyncClaude(ctx, wts, SyncAuto)
	if _, err := os.Lstat(filepath.Join(f.slot(1), ".claude", "rules")); err == nil {
		t.Fatal("emptied directory was left behind")
	}
}

func TestActivityPrefersClaudeTranscripts(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	f.branch("me/a")
	f.branch("me/b")
	a, _ := f.repo.Checkout(ctx, "me/a")
	b, err := f.repo.Checkout(ctx, "me/b")
	if err != nil {
		t.Fatal(err)
	}
	// Timestamps have one-second resolution, so pin them apart: a commit on
	// me/b puts it first until a Claude session touches me/a's worktree.
	f.write(b.Path, "b.txt", "b")
	f.git(b.Path, "add", ".")
	t.Setenv("GIT_COMMITTER_DATE", time.Now().Add(30*time.Second).Format(time.RFC3339))
	f.git(b.Path, "commit", "-qm", "b")
	if got := f.snap().CheckedOut(); got[0].Name != "me/b" {
		t.Fatalf("order = %v", names(got))
	}
	proj := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", claudeProjectKey(a.Path))
	f.write(proj, "session.jsonl", "{}")
	future := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(proj, "session.jsonl"), future, future)
	if got := f.snap().CheckedOut(); got[0].Name != "me/a" {
		t.Fatalf("order = %v, want me/a first", names(got))
	}
}

func TestClaudeProjectKey(t *testing.T) {
	if got := claudeProjectKey("/Users/x/code/utils/dev-cli"); got != "-Users-x-code-utils-dev-cli" {
		t.Fatal(got)
	}
	if got := claudeProjectKey("/a/b.c_d"); got != "-a-b-c-d" {
		t.Fatal(got)
	}
}

func TestParseTrack(t *testing.T) {
	cases := map[string]Branch{
		"":                  {},
		"gone":              {Gone: true},
		"ahead 2":           {Ahead: 2},
		"behind 3":          {Behind: 3},
		"ahead 1, behind 4": {Ahead: 1, Behind: 4},
	}
	for in, want := range cases {
		var got Branch
		parseTrack(&got, in)
		if got.Gone != want.Gone || got.Ahead != want.Ahead || got.Behind != want.Behind {
			t.Errorf("parseTrack(%q) = %+v", in, got)
		}
	}
}

func TestStatusCountsRenamesOnce(t *testing.T) {
	f := newFixture(t, 1)
	f.git(f.slot(1), "mv", "README.md", "READ ME.md")
	st := StatusOf(context.Background(), f.slot(1))
	if !st.Dirty || st.Files != 1 {
		t.Fatalf("status = %+v", st)
	}
}

func names(bs []*Branch) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name
	}
	return out
}
