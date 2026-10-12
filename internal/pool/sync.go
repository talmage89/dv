package pool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/talmage89/dv/internal/config"
	"github.com/talmage89/dv/internal/gitx"
)

// SyncMode controls how conflicts are handled.
type SyncMode int

const (
	// SyncAuto applies only changes that cannot lose data: creating missing
	// files and links, propagating a newer shared .env, removing dead links.
	// Anything else is reported as an issue.
	SyncAuto SyncMode = iota
	// SyncForce also overwrites diverged .env files and replaces regular files
	// that sit where a link belongs.
	SyncForce
)

// Severity of a sync issue.
type Severity int

const (
	SeverityWarn Severity = iota
	SeverityError
)

// SyncIssue is something sync could not or would not do.
type SyncIssue struct {
	Worktree string // directory name; empty for repo-wide issues
	Kind     string // "env" or "claude"
	Severity Severity
	Message  string
}

func (i SyncIssue) String() string {
	if i.Worktree == "" {
		return fmt.Sprintf("%s: %s", i.Kind, i.Message)
	}
	return fmt.Sprintf("%s %s: %s", i.Worktree, i.Kind, i.Message)
}

// SyncReport collects what a sync pass changed and what it could not do.
type SyncReport struct {
	Changes []string
	Issues  []SyncIssue
}

func (r *SyncReport) change(format string, args ...any) {
	r.Changes = append(r.Changes, fmt.Sprintf(format, args...))
}

func (r *SyncReport) issue(wt, kind string, sev Severity, format string, args ...any) {
	r.Issues = append(r.Issues, SyncIssue{Worktree: wt, Kind: kind, Severity: sev, Message: fmt.Sprintf(format, args...)})
}

// Merge appends other into r.
func (r *SyncReport) Merge(other SyncReport) {
	r.Changes = append(r.Changes, other.Changes...)
	r.Issues = append(r.Issues, other.Issues...)
}

// Sync brings the given worktrees in line with the shared .env and the main
// worktree's gitignored .claude files, per the config toggles.
func (r *Repo) Sync(ctx context.Context, worktrees []string, mode SyncMode) SyncReport {
	var rep SyncReport
	if r.Config.Bool(config.KeySyncEnv) {
		rep.Merge(r.SyncEnv(worktrees, mode))
	}
	if r.Config.Bool(config.KeySyncClaude) {
		rep.Merge(r.SyncClaude(ctx, worktrees, mode))
	}
	return rep
}

// SyncEnv writes each worktree's .env as the shared source followed by the
// configured extra line with {name} replaced by the worktree directory name.
//
// In auto mode a worktree .env that differs from the expected content is only
// overwritten when the source is newer than it; a worktree .env edited after
// the source changed is left alone and reported.
func (r *Repo) SyncEnv(worktrees []string, mode SyncMode) SyncReport {
	var rep SyncReport
	srcPath := r.Config.String(config.KeyEnvSource)
	if srcPath == "" {
		return rep
	}
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			rep.issue("", "env", SeverityWarn, "shared source %s does not exist; .env sync skipped (turn it off in config if unused)", srcPath)
		} else {
			rep.issue("", "env", SeverityError, "read %s: %v", srcPath, err)
		}
		return rep
	}
	src, err := os.ReadFile(srcPath)
	if err != nil {
		rep.issue("", "env", SeverityError, "read %s: %v", srcPath, err)
		return rep
	}
	line := r.Config.String(config.KeyEnvLine)
	for _, wt := range worktrees {
		name := baseName(wt)
		want := expectedEnv(src, line, name)
		dst := filepath.Join(wt, ".env")
		have, err := os.ReadFile(dst)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			rep.issue(name, "env", SeverityError, "read .env: %v", err)
			continue
		case bytes.Equal(have, want):
			continue
		case mode == SyncAuto:
			if info, err := os.Stat(dst); err == nil && !srcInfo.ModTime().After(info.ModTime()) {
				rep.issue(name, "env", SeverityWarn, ".env was edited after the shared source; force sync to overwrite")
				continue
			}
		}
		if err := writeFileAtomic(dst, want, 0o644); err != nil {
			rep.issue(name, "env", SeverityError, "write .env: %v", err)
			continue
		}
		rep.change("%s: wrote .env", name)
	}
	return rep
}

func expectedEnv(src []byte, line, name string) []byte {
	var b bytes.Buffer
	b.Write(src)
	if line == "" {
		return b.Bytes()
	}
	if len(src) > 0 && src[len(src)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString(strings.ReplaceAll(line, "{name}", name))
	b.WriteByte('\n')
	return b.Bytes()
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dv-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// SyncClaude links every gitignored file under the main worktree's .claude/
// into each other worktree's .claude/, and removes links whose source is gone.
// Tracked .claude files are left to git.
func (r *Repo) SyncClaude(ctx context.Context, worktrees []string, mode SyncMode) SyncReport {
	var rep SyncReport
	srcRoot := filepath.Join(r.Main, ".claude")
	files, err := r.ignoredClaudeFiles(ctx, srcRoot)
	if err != nil {
		rep.issue("", "claude", SeverityError, "%v", err)
		return rep
	}
	for _, wt := range worktrees {
		if wt == r.Main || realPath(wt) == realPath(r.Main) {
			continue
		}
		name := baseName(wt)
		dstRoot := filepath.Join(wt, ".claude")
		for _, rel := range files {
			src := filepath.Join(srcRoot, rel)
			dst := filepath.Join(dstRoot, rel)
			changed, err := linkOne(src, dst, mode)
			var conflict conflictError
			switch {
			case errors.As(err, &conflict):
				rep.issue(name, "claude", SeverityWarn, "%s is a regular file; force sync to replace it with a link", filepath.Join(".claude", rel))
			case err != nil:
				rep.issue(name, "claude", SeverityError, "link %s: %v", rel, err)
			case changed:
				rep.change("%s: linked .claude/%s", name, rel)
			}
		}
		removeDeadLinks(&rep, name, srcRoot, dstRoot)
	}
	return rep
}

// ignoredClaudeFiles lists regular files under srcRoot (relative paths) that
// the main worktree's gitignore rules ignore.
func (r *Repo) ignoredClaudeFiles(ctx context.Context, srcRoot string) ([]string, error) {
	var rels []string
	err := filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(srcRoot, path)
			rels = append(rels, rel)
		}
		return nil
	})
	if err != nil || len(rels) == 0 {
		return nil, err
	}
	var in bytes.Buffer
	for _, rel := range rels {
		in.WriteString(filepath.ToSlash(filepath.Join(".claude", rel)))
		in.WriteByte(0)
	}
	out, err := gitx.RunInput(ctx, r.Main, in.Bytes(), "check-ignore", "--stdin", "-z")
	if err != nil && gitx.ExitCode(err) != 1 { // exit 1: nothing ignored
		return nil, fmt.Errorf("check-ignore: %s", gitx.Message(err))
	}
	var ignored []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			ignored = append(ignored, filepath.FromSlash(strings.TrimPrefix(string(p), ".claude/")))
		}
	}
	sort.Strings(ignored)
	return ignored, nil
}

type conflictError struct{ path string }

func (e conflictError) Error() string { return e.path + " is a regular file" }

// linkOne ensures dst is a symlink to src, reporting whether it changed
// anything.
func linkOne(src, dst string, mode SyncMode) (bool, error) {
	info, err := os.Lstat(dst)
	switch {
	case err == nil && info.Mode()&fs.ModeSymlink != 0:
		if target, _ := os.Readlink(dst); target == src {
			return false, nil
		}
		// A link pointing elsewhere holds no data of its own; repoint it.
		if err := os.Remove(dst); err != nil {
			return false, err
		}
	case err == nil && info.IsDir():
		return false, conflictError{dst}
	case err == nil:
		if mode != SyncForce {
			return false, conflictError{dst}
		}
		if err := os.Remove(dst); err != nil {
			return false, err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	if err := os.Symlink(src, dst); err != nil {
		return false, err
	}
	return true, nil
}

// removeDeadLinks deletes symlinks under dstRoot that point into srcRoot at
// files that no longer exist, then removes directories those deletions left
// empty. Regular files and pre-existing empty directories are never touched.
func removeDeadLinks(rep *SyncReport, name, srcRoot, dstRoot string) {
	var removed []string
	_ = filepath.WalkDir(dstRoot, func(path string, d fs.DirEntry, walkErr error) error {
		// Unreadable entries are skipped rather than aborting the walk.
		if walkErr != nil || d.Type()&fs.ModeSymlink == 0 {
			return nil //nolint:nilerr
		}
		target, readErr := os.Readlink(path)
		if readErr != nil || !strings.HasPrefix(target, srcRoot+string(filepath.Separator)) {
			return nil //nolint:nilerr
		}
		if _, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) && os.Remove(path) == nil {
			rel, _ := filepath.Rel(dstRoot, path)
			rep.change("%s: removed dead link .claude/%s", name, rel)
			removed = append(removed, path)
		}
		return nil
	})
	for _, p := range removed {
		for dir := filepath.Dir(p); dir != dstRoot && strings.HasPrefix(dir, dstRoot); dir = filepath.Dir(dir) {
			if os.Remove(dir) != nil { // fails unless empty
				break
			}
		}
	}
}
