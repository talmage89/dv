// Package testrepo builds throwaway repositories laid out like a dv setup for
// tests in other packages.
package testrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Repo is a bare origin, a main worktree at Root/app, and pool slots
// Root/dev-1..n detached at origin/main.
type Repo struct {
	T    testing.TB
	Dir  string
	Root string
	Main string
}

// New creates the repository and points DEV_REPO at it. Git is isolated from
// the developer's own config.
func New(t testing.TB, slots int) *Repo {
	t.Helper()
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
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	r := &Repo{T: t, Dir: dir, Root: filepath.Join(dir, "root")}
	r.Main = filepath.Join(r.Root, "app")
	origin := filepath.Join(dir, "origin.git")
	r.Git(dir, "init", "--bare", "-q", origin)
	r.Git(dir, "clone", "-q", origin, r.Main)
	r.Write(r.Main, "README.md", "hello\n")
	r.Write(r.Main, ".gitignore", ".env\n")
	r.Git(r.Main, "add", ".")
	r.Git(r.Main, "commit", "-qm", "init")
	r.Git(r.Main, "push", "-q", "origin", "main")
	for i := 1; i <= slots; i++ {
		r.Git(r.Main, "worktree", "add", "-q", "--detach", r.Slot(i), "origin/main")
	}
	t.Setenv("DEV_REPO", r.Main)
	t.Setenv("DEV_ROOT", r.Root)
	t.Setenv("DEV_USER", "me")
	return r
}

// Slot returns the path of pool slot n.
func (r *Repo) Slot(n int) string { return filepath.Join(r.Root, "dev-"+strconv.Itoa(n)) }

// Git runs git in dir and returns trimmed output, failing the test on error.
func (r *Repo) Git(dir string, args ...string) string {
	r.T.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.T.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Write creates a file under dir.
func (r *Repo) Write(dir, rel, content string) {
	r.T.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.T.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.T.Fatal(err)
	}
}

// Checkout checks branch out (creating it from origin/main) in slot n.
func (r *Repo) Checkout(n int, branch string) {
	r.T.Helper()
	r.Git(r.Slot(n), "checkout", "-q", "-b", branch, "origin/main")
}
