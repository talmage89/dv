package pool

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// fillActivity estimates when each branch was last worked on. Commit times
// alone miss most of the signal: long-running Claude sessions and uncommitted
// work can go days without a commit. The estimate is the latest of
//
//   - the branch ref's own reflog (commits, resets, rebases anywhere),
//   - the holding worktree's HEAD reflog (checkouts, commits there),
//   - Claude Code transcript activity in the holding worktree, counted only
//     after the branch was checked out there so a previous occupant's sessions
//     are not attributed to it,
//   - the tip commit time, as a floor.
func (r *Repo) fillActivity(snap *Snapshot) {
	claude := claudeProjectsDir()
	for _, b := range snap.Branches {
		last := b.CommitTime
		if t, _ := lastReflogEntry(filepath.Join(r.CommonDir, "logs", "refs", "heads", filepath.FromSlash(b.Name)), ""); t.After(last) {
			last = t
		}
		if b.Worktree != "" {
			admin := adminDir(b.Worktree, r.CommonDir)
			headLast, checkedOut := lastReflogEntry(filepath.Join(admin, "logs", "HEAD"), b.Name)
			if headLast.After(last) {
				last = headLast
			}
			if claude != "" {
				if t := claudeActivity(claude, b.Worktree); t.After(last) && !t.Before(checkedOut) {
					last = t
				}
			}
		}
		b.LastActive = last
	}
}

// adminDir returns a worktree's private git directory (where its HEAD and
// HEAD reflog live). Linked worktrees have a ".git" file pointing at it; the
// main worktree's is the common dir.
func adminDir(worktree, commonDir string) string {
	data, err := os.ReadFile(filepath.Join(worktree, ".git"))
	if err != nil {
		return commonDir // a directory (main worktree) or unreadable
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return commonDir
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(worktree, gitdir)
	}
	return gitdir
}

// reflogTail bounds how much of a reflog is read. HEAD reflogs of busy
// worktrees grow without bound; the most recent entries are all that matter.
const reflogTail = 64 << 10

// lastReflogEntry returns the time of a reflog's newest entry and, when
// branch is non-empty, the time of the newest entry that checked branch out.
func lastReflogEntry(path, branch string) (last, checkedOut time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > reflogTail {
		if _, err := f.Seek(-reflogTail, io.SeekEnd); err != nil {
			return
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	moveTo := []byte("checkout: moving from ")
	target := []byte(" to " + branch)
	for i := len(lines) - 1; i >= 0; i-- {
		ts, msg, ok := parseReflogLine(lines[i])
		if !ok {
			continue
		}
		if last.IsZero() {
			last = ts
		}
		if branch == "" {
			return
		}
		if bytes.HasPrefix(msg, moveTo) && bytes.HasSuffix(msg, target) {
			checkedOut = ts
			return
		}
	}
	return
}

// parseReflogLine parses "<old> <new> <name> <<email>> <unix> <tz>\t<msg>".
func parseReflogLine(line []byte) (time.Time, []byte, bool) {
	head, msg, _ := bytes.Cut(line, []byte("\t"))
	fields := bytes.Fields(head)
	if len(fields) < 2 {
		return time.Time{}, nil, false
	}
	ts, err := strconv.ParseInt(string(fields[len(fields)-2]), 10, 64)
	if err != nil {
		return time.Time{}, nil, false
	}
	return time.Unix(ts, 0), msg, true
}

// claudeProjectsDir is where Claude Code stores per-directory transcripts.
func claudeProjectsDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// claudeProjectKey mirrors how Claude Code names a directory's transcript
// folder: every character outside [A-Za-z0-9] becomes '-'.
func claudeProjectKey(path string) string {
	var b strings.Builder
	for _, c := range path {
		if c < 128 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// claudeActivity returns the newest transcript modification time for
// sessions started in worktree or any directory beneath it.
func claudeActivity(projectsDir, worktree string) time.Time {
	key := claudeProjectKey(worktree)
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return time.Time{}
	}
	var newest time.Time
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || (name != key && !strings.HasPrefix(name, key+"-")) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(projectsDir, name))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			if info, err := f.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
	}
	return newest
}
