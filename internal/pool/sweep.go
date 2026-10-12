package pool

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/talmage89/dv/internal/gitx"
)

// SweepCandidate is a worktree holding a branch that is not the user's.
type SweepCandidate struct {
	Branch   string
	Worktree string
	Status   Status
	Live     int // Claude sessions running in the worktree
}

// Selectable reports whether the worktree can be freed without losing work.
func (c SweepCandidate) Selectable() bool { return c.Status.Err == "" && !c.Status.Dirty }

// DefaultSelected is the initial selection: worktrees no Claude session is
// using. Live ones must be opted into.
func (c SweepCandidate) DefaultSelected() bool { return c.Selectable() && c.Live == 0 }

// SweepCandidates lists pooled and external worktrees whose branch is not the
// user's: none of the commits it has beyond the base ref is authored by one of
// the user's emails. A branch with no such commits (merged, or never committed
// to) is judged by its tip's author. The branch the base ref tracks is exempt.
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
		if branch.Name == baseBranch {
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
	var paths []string
	for _, branch := range others {
		paths = append(paths, branch.Worktree)
	}
	live := LiveSessions(ctx, paths)
	out := make([]SweepCandidate, 0, len(others))
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
