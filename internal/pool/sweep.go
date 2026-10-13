package pool

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/talmage89/dv/internal/gitx"
)

// SweepCandidate is a branch that sweep clears away. A branch whose upstream
// was deleted on origin is deleted outright; a worktree holding a branch that
// is not the user's is freed, keeping the branch.
type SweepCandidate struct {
	Branch   string
	Worktree string // holding worktree, if checked out
	Status   Status // of the holding worktree
	Live     int    // Claude sessions running in the holding worktree
	Gone     bool   // upstream deleted on origin; sweeping deletes the branch
	Parked   bool
	InMain   bool // checked out in the main worktree; never swept
}

// Selectable reports whether the candidate may be swept at all. Deleting a
// gone branch can discard its worktree's changes; freeing a kept branch never
// discards anything, so a dirty worktree rules it out.
func (c SweepCandidate) Selectable() bool {
	return !c.InMain && c.Status.Err == "" && (c.Gone || !c.Status.Dirty)
}

// DefaultSelected is the initial selection: everything safe. Uncommitted
// changes, parked branches, and running Claude sessions must be opted into.
func (c SweepCandidate) DefaultSelected() bool {
	return c.Selectable() && !c.Status.Dirty && !c.Parked && c.Live == 0
}

// Fetch updates remote-tracking refs and drops ones deleted on the remote.
func (r *Repo) Fetch(ctx context.Context) error {
	if _, err := gitx.Run(ctx, r.Main, "fetch", "--prune", "--quiet", "origin"); err != nil {
		return fmt.Errorf("fetch: %s", gitx.Message(err))
	}
	return nil
}

// SweepCandidates lists branches whose upstream is gone as of the last fetch,
// then checked-out branches that are not the user's: none of the commits a
// branch has beyond the base ref is authored by one of the user's emails. A
// branch with no such commits (merged, or never committed to) is judged by its
// tip's author. The branch the base ref tracks is never the user's to sweep.
func (r *Repo) SweepCandidates(ctx context.Context) ([]SweepCandidate, error) {
	if len(r.Emails) == 0 {
		return nil, fmt.Errorf("no emails configured; set them with `dv config emails <a>,<b>`")
	}
	if !r.refExists(ctx, r.Base) {
		return nil, fmt.Errorf("base ref %s not found; it decides which commits a branch carries", r.Base)
	}
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	gone := snap.Gone()
	others, err := r.othersBranches(ctx, snap)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, branch := range append(append([]*Branch{}, gone...), others...) {
		if branch.Worktree != "" {
			paths = append(paths, branch.Worktree)
		}
	}
	live := LiveSessions(ctx, paths)
	out := make([]SweepCandidate, 0, len(gone)+len(others))
	for _, branch := range gone {
		c := SweepCandidate{Branch: branch.Name, Worktree: branch.Worktree, Gone: true, Parked: branch.Parked}
		if branch.Worktree != "" {
			c.InMain = branch.Worktree == snap.Main.Path
			c.Status = StatusOf(ctx, branch.Worktree)
			c.Live = live[branch.Worktree]
		}
		out = append(out, c)
	}
	for _, branch := range others {
		out = append(out, SweepCandidate{
			Branch:   branch.Name,
			Worktree: branch.Worktree,
			Status:   StatusOf(ctx, branch.Worktree),
			Live:     live[branch.Worktree],
		})
	}
	return out, nil
}

// othersBranches returns the checked-out branches that are not the user's,
// leaving out gone ones, which sweep deletes instead.
func (r *Repo) othersBranches(ctx context.Context, snap *Snapshot) ([]*Branch, error) {
	baseBranch := r.Base
	if r.refExists(ctx, "refs/remotes/"+r.Base) {
		_, baseBranch, _ = strings.Cut(r.Base, "/")
	}
	var (
		others []*Branch
		mu     sync.Mutex
		wg     sync.WaitGroup
		failed error
	)
	for _, branch := range snap.CheckedOut() {
		if branch.Name == baseBranch || branch.Gone {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			authors, err := r.branchAuthors(ctx, branch.Name)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failed = err
			case !r.authoredByUser(authors):
				others = append(others, branch)
			}
		}()
	}
	wg.Wait()
	if failed != nil {
		return nil, failed
	}
	sortByActivity(others)
	return others, nil
}

// Sweep deletes a gone branch, freeing its worktree first, or frees the
// worktree of a branch that is not the user's. With discard, a gone branch's
// uncommitted changes are thrown away; without it, they are an error.
func (r *Repo) Sweep(ctx context.Context, c SweepCandidate, discardChanges bool) error {
	if c.InMain {
		return fmt.Errorf("%s: %w", c.Branch, ErrInMain)
	}
	if !c.Gone {
		return r.Release(ctx, c.Branch)
	}
	if c.Worktree != "" && discardChanges {
		if err := discard(ctx, c.Worktree); err != nil {
			return fmt.Errorf("discard changes: %s", gitx.Message(err))
		}
	}
	return r.Delete(ctx, c.Branch)
}

// branchAuthors returns the author emails of a branch's commits beyond the
// base ref, or of its tip when it has none.
func (r *Repo) branchAuthors(ctx context.Context, branch string) ([]string, error) {
	ref := "refs/heads/" + branch
	out, err := gitx.Run(ctx, r.Main, "log", "--format=%ae", r.Base+".."+ref)
	if err == nil && out == "" {
		out, err = gitx.Run(ctx, r.Main, "log", "-1", "--format=%ae", ref)
	}
	if err != nil {
		return nil, fmt.Errorf("authors of %s: %s", branch, gitx.Message(err))
	}
	return strings.Split(out, "\n"), nil
}

func (r *Repo) authoredByUser(authors []string) bool {
	for _, author := range authors {
		for _, email := range r.Emails {
			if strings.EqualFold(author, email) {
				return true
			}
		}
	}
	return false
}

func splitEmails(list string) []string {
	var emails []string
	for _, email := range strings.Split(list, ",") {
		if email = strings.TrimSpace(email); email != "" {
			emails = append(emails, email)
		}
	}
	return emails
}
