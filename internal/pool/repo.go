// Package pool models a repository's worktrees as an anonymous pool of slots.
// A slot is a worktree directory under the root named <prefix><n>. Which slot
// holds a branch is an implementation detail: callers ask for a branch and the
// pool finds (or frees up) a place for it.
package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/talmage89/dv/internal/config"
	"github.com/talmage89/dv/internal/gitx"
)

// Repo is a resolved target repository.
type Repo struct {
	Main      string   // main worktree path
	Root      string   // directory holding the pool's slots
	Prefix    string   // slot directory name prefix, e.g. "dev-"
	User      string   // branch namespace treated as the user's own
	Base      string   // ref new branches start from, e.g. "origin/main"
	Emails    []string // commit author emails that make a branch the user's own
	CommonDir string   // absolute path of the shared .git directory
	Config    *config.Config

	rootReal string // Root with symlinks resolved, for path comparisons
}

// ErrNoRepo means no target repository could be resolved.
var ErrNoRepo = errors.New("no target repo configured")

// Open resolves the target repository from cfg.
func Open(ctx context.Context, cfg *config.Config) (*Repo, error) {
	hint := cfg.String(config.KeyRepo)
	if hint == "" {
		return nil, ErrNoRepo
	}
	if _, err := os.Stat(hint); err != nil {
		return nil, fmt.Errorf("repo %s: %w", hint, err)
	}
	common, err := gitx.Run(ctx, hint, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %s", hint, gitx.Message(err))
	}
	out, err := gitx.Run(ctx, hint, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	wts := gitx.ParseWorktrees(out)
	if len(wts) == 0 || wts[0].Bare {
		return nil, fmt.Errorf("%s: bare repositories are not supported", hint)
	}
	cfg.MainHint = wts[0].Path
	r := &Repo{
		Main:      wts[0].Path,
		Root:      cfg.String(config.KeyRoot),
		Prefix:    cfg.String(config.KeyPrefix),
		User:      cfg.String(config.KeyUser),
		Base:      cfg.String(config.KeyBase),
		Emails:    splitEmails(cfg.String(config.KeyEmails)),
		CommonDir: filepath.Clean(common),
		Config:    cfg,
	}
	if r.Prefix == "" {
		r.Prefix = "dev-"
	}
	r.Root = filepath.Clean(r.Root)
	r.rootReal = realPath(r.Root)
	return r, nil
}

// Name is the repository's display name (the main worktree's directory).
func (r *Repo) Name() string { return filepath.Base(r.Main) }

// slotNum parses the slot number from a directory path, or returns -1 if the
// path is not a pool slot.
func (r *Repo) slotNum(path string) int {
	if dir := filepath.Dir(path); dir != r.Root && realPath(dir) != r.rootReal {
		return -1
	}
	rest, ok := strings.CutPrefix(filepath.Base(path), r.Prefix)
	if !ok || rest == "" || (len(rest) > 1 && rest[0] == '0') {
		return -1
	}
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// SlotPath returns the directory for slot n.
func (r *Repo) SlotPath(n int) string {
	return filepath.Join(r.Root, fmt.Sprintf("%s%d", r.Prefix, n))
}

// refExists reports whether ref resolves to a commit.
func (r *Repo) refExists(ctx context.Context, ref string) bool {
	_, err := gitx.Run(ctx, r.Main, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// realPath resolves symlinks in p, returning p unchanged if it cannot.
func realPath(p string) string {
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	return p
}
