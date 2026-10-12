// Package gitx runs git commands and parses the porcelain formats dv relies on.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Error is returned when git exits non-zero. Stderr carries git's own message,
// which is usually the most useful thing to show the user.
type Error struct {
	Args   []string
	Dir    string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), firstLines(msg, 3))
}

func (e *Error) Unwrap() error { return e.Err }

// Message is git's stderr without the "git <args>:" prefix, for UI surfaces
// that already say which operation failed.
func Message(err error) string {
	var ge *Error
	if errors.As(err, &ge) {
		if msg := strings.TrimSpace(ge.Stderr); msg != "" {
			return firstLines(msg, 3)
		}
		return ge.Err.Error()
	}
	return err.Error()
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// baseEnv keeps git from ever blocking on a credential prompt and from taking
// optional locks. dv reads worktrees while other git processes (editors,
// Claude sessions) work in them; an opportunistic index.lock taken by a
// background `git status` can make those processes fail.
func baseEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	)
}

// Run executes git in dir and returns stdout with the trailing newline trimmed.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := RunRaw(ctx, dir, args...)
	return strings.TrimRight(string(out), "\n"), err
}

// RunRaw executes git in dir and returns stdout untouched.
func RunRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return RunInput(ctx, dir, nil, args...)
}

// RunInput executes git with the given stdin.
func RunInput(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = baseEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &Error{Args: args, Dir: dir, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

// ExitCode returns git's exit status from an error returned by Run, or -1.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// Worktree is one entry of `git worktree list --porcelain`.
type Worktree struct {
	Path     string
	Head     string
	Branch   string // short name; empty when detached
	Detached bool
	Bare     bool
	Locked   bool
	Prunable bool
}

// ParseWorktrees parses `git worktree list --porcelain` output. The main
// worktree is always first.
func ParseWorktrees(out string) []Worktree {
	var (
		wts []Worktree
		cur *Worktree
	)
	flush := func() {
		if cur != nil {
			wts = append(wts, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &Worktree{Path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "detached":
			cur.Detached = true
		case line == "bare":
			cur.Bare = true
		case line == "locked" || strings.HasPrefix(line, "locked "):
			cur.Locked = true
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			cur.Prunable = true
		}
	}
	flush()
	return wts
}

// ValidBranchName reports whether name is acceptable as a new branch name,
// using git's own rules.
func ValidBranchName(ctx context.Context, name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	_, err := Run(ctx, "", "check-ref-format", "--branch", name)
	return err == nil
}
