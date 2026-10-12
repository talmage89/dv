package pool

import (
	"context"
	"fmt"

	"github.com/talmage89/dv/internal/gitx"
)

// PruneCandidate is a local branch whose upstream was deleted on origin.
type PruneCandidate struct {
	Branch   string
	Worktree string // holding worktree, if checked out
	Status   Status // of the holding worktree
	Live     int    // Claude sessions running in the holding worktree
	Parked   bool
	InMain   bool // checked out in the main worktree; never pruned
}

// Selectable reports whether the candidate may be pruned at all.
func (c PruneCandidate) Selectable() bool { return !c.InMain && c.Status.Err == "" }

// DefaultSelected is the initial selection: everything safe. Dirty worktrees
// and parked branches must be opted into.
func (c PruneCandidate) DefaultSelected() bool {
	return c.Selectable() && !c.Status.Dirty && !c.Parked
}

// Fetch updates remote-tracking refs and drops ones deleted on the remote.
func (r *Repo) Fetch(ctx context.Context) error {
	if _, err := gitx.Run(ctx, r.Main, "fetch", "--prune", "--quiet", "origin"); err != nil {
		return fmt.Errorf("fetch: %s", gitx.Message(err))
	}
	return nil
}

// PruneCandidates lists branches whose upstream is gone, as of the last fetch.
func (r *Repo) PruneCandidates(ctx context.Context) ([]PruneCandidate, error) {
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	gone := snap.Gone()
	var paths []string
	for _, b := range gone {
		if b.Worktree != "" {
			paths = append(paths, b.Worktree)
		}
	}
	live := LiveSessions(ctx, paths)
	out := make([]PruneCandidate, 0, len(gone))
	for _, b := range gone {
		c := PruneCandidate{Branch: b.Name, Worktree: b.Worktree, Parked: b.Parked}
		if b.Worktree != "" {
			c.InMain = b.Worktree == snap.Main.Path
			c.Status = StatusOf(ctx, b.Worktree)
			c.Live = live[b.Worktree]
		}
		out = append(out, c)
	}
	return out, nil
}

// Prune deletes one gone branch, freeing its worktree first. With discard, a
// dirty worktree's changes are thrown away; without it, a dirty worktree is
// an error.
func (r *Repo) Prune(ctx context.Context, c PruneCandidate, discardChanges bool) error {
	if c.InMain {
		return fmt.Errorf("%s: %w", c.Branch, ErrInMain)
	}
	if c.Worktree != "" && discardChanges {
		if err := discard(ctx, c.Worktree); err != nil {
			return fmt.Errorf("discard changes: %s", gitx.Message(err))
		}
	}
	return r.Delete(ctx, c.Branch)
}
