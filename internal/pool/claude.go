package pool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Session is a running Claude Code session.
type Session struct {
	Name       string `json:"name,omitempty"`
	Background bool   `json:"background,omitempty"`
	Status     string `json:"status,omitempty"`     // busy, waiting, or idle; empty when unknown
	WaitingFor string `json:"waitingFor,omitempty"` // what a waiting session needs
	State      string `json:"state,omitempty"`      // background only: working, blocked, done, failed, or stopped
}

// Activity is one word for what the session is doing: waiting, busy, idle,
// done, failed, or stopped, or running when Claude Code did not say.
func (s Session) Activity() string {
	switch {
	case s.State == "done" || s.State == "failed" || s.State == "stopped":
		return s.State
	case s.Status == "waiting" || s.State == "blocked":
		return "waiting"
	case s.Status == "busy" || s.State == "working":
		return "busy"
	case s.Status == "idle":
		return "idle"
	}
	return "running"
}

// ClaudeInstalled reports whether the claude command is on the PATH.
func ClaudeInstalled() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

type locatedSession struct {
	cwd     string
	session Session
}

// Sessions maps worktree path to the Claude Code sessions whose working
// directory is inside it. Claude Code reports what each session is doing;
// when it cannot, running claude processes are found without that detail.
func Sessions(ctx context.Context, worktrees []string) map[string][]Session {
	located, err := agentSessions(ctx)
	if err != nil {
		located = processSessions(ctx)
	}
	sessions := map[string][]Session{}
	for _, ls := range located {
		for _, wt := range worktrees {
			if ls.cwd == wt || strings.HasPrefix(ls.cwd, wt+string(filepath.Separator)) {
				sessions[wt] = append(sessions[wt], ls.session)
				break
			}
		}
	}
	return sessions
}

// agentSessions reads `claude agents --json`, which lists interactive and
// background sessions with their status.
func agentSessions(ctx context.Context) ([]locatedSession, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "agents", "--json").Output()
	if err != nil {
		return nil, err
	}
	var agents []struct {
		Cwd        string `json:"cwd"`
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		WaitingFor string `json:"waitingFor"`
		State      string `json:"state"`
	}
	if err := json.Unmarshal(out, &agents); err != nil {
		return nil, err
	}
	located := make([]locatedSession, 0, len(agents))
	for _, agent := range agents {
		located = append(located, locatedSession{cwd: agent.Cwd, session: Session{
			Name:       agent.Name,
			Background: agent.Kind == "background",
			Status:     agent.Status,
			WaitingFor: agent.WaitingFor,
			State:      agent.State,
		}})
	}
	return located, nil
}

// processSessions finds running `claude` processes by their working
// directories, for Claude Code versions without `claude agents --json`.
func processSessions(ctx context.Context) []locatedSession {
	var located []locatedSession
	for _, cwd := range claudeCwds(ctx) {
		located = append(located, locatedSession{cwd: cwd})
	}
	return located
}

// claudeCwds lists the working directories of running `claude` processes.
func claudeCwds(ctx context.Context) []string {
	pidsOut, err := exec.CommandContext(ctx, "pgrep", "-x", "claude").Output()
	if err != nil {
		return nil // exit 1: no matching processes
	}
	pids := strings.Fields(string(pidsOut))
	if len(pids) == 0 {
		return nil
	}
	if runtime.GOOS == "linux" {
		var cwds []string
		for _, pid := range pids {
			if cwd, err := os.Readlink("/proc/" + pid + "/cwd"); err == nil {
				cwds = append(cwds, cwd)
			}
		}
		return cwds
	}
	out, _ := exec.CommandContext(ctx, "lsof", "-a", "-p", strings.Join(pids, ","), "-d", "cwd", "-Fn").Output()
	var cwds []string
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "n"); ok {
			cwds = append(cwds, p)
		}
	}
	return cwds
}
