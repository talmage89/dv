package pool

import (
	"bytes"
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/talmage89/dv/internal/gitx"
)

// Status is a worktree's uncommitted state.
type Status struct {
	Dirty     bool
	Files     int // changed + untracked paths
	Untracked int
	Err       string // non-empty when git status failed (locked index, ...)
	CheckedAt time.Time
}

// Known reports whether the status was ever computed.
func (s Status) Known() bool { return !s.CheckedAt.IsZero() }

// StatusOf runs `git status` in a worktree. It never takes the index lock.
func StatusOf(ctx context.Context, path string) Status {
	out, err := gitx.RunRaw(ctx, path, "status", "--porcelain=v1", "-z", "--untracked-files=normal", "--ignore-submodules=dirty")
	st := Status{CheckedAt: time.Now()}
	if err != nil {
		st.Err = gitx.Message(err)
		return st
	}
	recs := bytes.Split(out, []byte{0})
	for i := 0; i < len(recs); i++ {
		rec := recs[i]
		if len(rec) < 4 {
			continue
		}
		// Renames and copies are followed by a record holding the source path.
		if rec[0] == 'R' || rec[0] == 'C' || rec[1] == 'R' || rec[1] == 'C' {
			i++
		}
		st.Files++
		if rec[0] == '?' {
			st.Untracked++
		}
	}
	st.Dirty = st.Files > 0
	return st
}

// StatusAll computes statuses for paths with bounded concurrency, calling
// onEach as each finishes. Parallel `git status` across many worktrees of a
// large repo thrashes the filesystem, so concurrency stays low.
func StatusAll(ctx context.Context, paths []string, onEach func(path string, st Status)) {
	sem := make(chan struct{}, statusConcurrency())
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			onEach(p, StatusOf(ctx, p))
		}()
	}
	wg.Wait()
}

func statusConcurrency() int {
	return min(max(runtime.NumCPU()/3, 2), 4)
}
