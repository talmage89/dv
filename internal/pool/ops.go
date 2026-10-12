package pool

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/talmage89/dv/internal/gitx"
)

var (
	// ErrNoFreeSlot means every pool slot holds a branch, has uncommitted
	// changes, or has a live Claude session.
	ErrNoFreeSlot = errors.New("no free worktree: every one holds a branch, has changes, or has a Claude session running")
	// ErrBranchNotFound means the branch exists neither locally nor on origin.
	ErrBranchNotFound = errors.New("branch not found locally or on origin")
	// ErrBranchExists means a new branch's name is taken.
	ErrBranchExists = errors.New("branch already exists")
	// ErrDirty means an operation would lose uncommitted changes.
	ErrDirty = errors.New("worktree has uncommitted changes")
	// ErrInMain means an operation would change the main worktree.
	ErrInMain = errors.New("branch is checked out in the main worktree")
)

// FreeSlot returns the lowest-numbered slot that is detached, clean, present
// on disk, and has no running Claude session. Cleanliness is checked fresh:
// the caller is about to check something out into it.
func (r *Repo) FreeSlot(ctx context.Context, snap *Snapshot) (Slot, error) {
	var candidates []string
	for _, s := range snap.Slots {
		if s.Detached() && !s.Missing && !s.Locked {
			candidates = append(candidates, s.Path)
		}
	}
	live := LiveSessions(ctx, candidates)
	for _, s := range snap.Slots {
		if !s.Detached() || s.Missing || s.Locked || live[s.Path] > 0 {
			continue
		}
		if st := StatusOf(ctx, s.Path); st.Err == "" && !st.Dirty {
			return s, nil
		}
	}
	return Slot{}, ErrNoFreeSlot
}

// CheckoutResult describes where a branch ended up.
type CheckoutResult struct {
	Path    string
	Moved   bool // false when the branch was already checked out
	Tracked bool // true when a local branch was created from origin
}

// Checkout returns the worktree holding branch, checking it out into a free
// slot first if needed. A branch that only exists on origin is fetched and
// created as a tracking branch. Checking out a parked branch unparks it.
func (r *Repo) Checkout(ctx context.Context, branch string) (CheckoutResult, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return CheckoutResult{}, errors.New("empty branch name")
	}
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return CheckoutResult{}, err
	}
	if s, ok := snap.SlotFor(branch); ok {
		return CheckoutResult{Path: s.Path}, nil
	}
	local := snap.Branches[branch] != nil
	remoteRef := "refs/remotes/origin/" + branch
	if !local && !r.refExists(ctx, remoteRef) {
		// A teammate's freshly pushed branch: fetch just that ref.
		_, _ = gitx.Run(ctx, r.Main, "fetch", "origin", branch)
		if !r.refExists(ctx, remoteRef) {
			return CheckoutResult{}, fmt.Errorf("%s: %w", branch, ErrBranchNotFound)
		}
	}
	slot, err := r.FreeSlot(ctx, snap)
	if err != nil {
		return CheckoutResult{}, err
	}
	res := CheckoutResult{Path: slot.Path, Moved: true}
	if local {
		_, err = gitx.Run(ctx, slot.Path, "checkout", branch)
	} else {
		_, err = gitx.Run(ctx, slot.Path, "checkout", "-b", branch, "--track", "origin/"+branch)
		res.Tracked = true
	}
	if err != nil {
		return CheckoutResult{}, fmt.Errorf("check out %s: %s", branch, gitx.Message(err))
	}
	if b := snap.Branches[branch]; b != nil && b.Parked {
		_ = r.Unpark(ctx, branch)
	}
	return res, nil
}

// NewBranch fetches the base ref and creates branch from it in a free slot.
// The branch does not track the base; its upstream is set on first push.
func (r *Repo) NewBranch(ctx context.Context, branch, base string) (string, error) {
	branch = strings.TrimSpace(branch)
	if base == "" {
		base = r.Base
	}
	if !gitx.ValidBranchName(ctx, branch) {
		return "", fmt.Errorf("%q is not a valid branch name", branch)
	}
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	if snap.Branches[branch] != nil {
		return "", fmt.Errorf("%s: %w", branch, ErrBranchExists)
	}
	if err := r.fetchBase(ctx, base); err != nil {
		return "", err
	}
	slot, err := r.FreeSlot(ctx, snap)
	if err != nil {
		return "", err
	}
	if _, err := gitx.Run(ctx, slot.Path, "checkout", "--no-track", "-b", branch, base); err != nil {
		return "", fmt.Errorf("create %s: %s", branch, gitx.Message(err))
	}
	return slot.Path, nil
}

// Fresh fetches the base ref and detaches a free slot at it, for work that
// has no branch yet. The slot stays free until it has changes or a Claude
// session.
func (r *Repo) Fresh(ctx context.Context) (string, error) {
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	if err := r.fetchBase(ctx, r.Base); err != nil {
		return "", err
	}
	slot, err := r.FreeSlot(ctx, snap)
	if err != nil {
		return "", err
	}
	if _, err := gitx.Run(ctx, slot.Path, "checkout", "--detach", r.Base); err != nil {
		return "", fmt.Errorf("detach at %s: %s", r.Base, gitx.Message(err))
	}
	return slot.Path, nil
}

// fetchBase updates base from its remote when it names one, and confirms it
// resolves.
func (r *Repo) fetchBase(ctx context.Context, base string) error {
	if remote, rb, ok := strings.Cut(base, "/"); ok && r.isRemote(ctx, remote) {
		if _, err := gitx.Run(ctx, r.Main, "fetch", remote, rb); err != nil {
			return fmt.Errorf("fetch %s: %s", base, gitx.Message(err))
		}
	}
	if !r.refExists(ctx, base) {
		return fmt.Errorf("base %s does not exist", base)
	}
	return nil
}

func (r *Repo) isRemote(ctx context.Context, name string) bool {
	_, err := gitx.Run(ctx, r.Main, "config", "--get", "remote."+name+".url")
	return err == nil
}

// release detaches the worktree holding branch so the slot becomes free. The
// worktree must be clean. It is detached at the base ref so the next branch
// checked out there starts from a recent tree.
func (r *Repo) release(ctx context.Context, snap *Snapshot, branch string) error {
	s, ok := snap.SlotFor(branch)
	if !ok {
		return nil
	}
	if s.Main {
		return fmt.Errorf("%s: %w", branch, ErrInMain)
	}
	st := StatusOf(ctx, s.Path)
	if st.Err != "" {
		return fmt.Errorf("status of %s: %s", branch, st.Err)
	}
	if st.Dirty {
		return fmt.Errorf("%s: %w (%d files)", branch, ErrDirty, st.Files)
	}
	target := "HEAD"
	if r.refExists(ctx, r.Base) {
		target = r.Base
	}
	if _, err := gitx.Run(ctx, s.Path, "checkout", "--detach", target); err != nil {
		return fmt.Errorf("detach %s: %s", branch, gitx.Message(err))
	}
	return nil
}

// Release frees the slot holding branch without deleting or parking it.
func (r *Repo) Release(ctx context.Context, branch string) error {
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return err
	}
	return r.release(ctx, snap, branch)
}

// Park marks branch as parked and frees its slot. Parked branches stay listed
// so they are easy to come back to, but hold no worktree. A branch with
// uncommitted changes cannot be parked.
func (r *Repo) Park(ctx context.Context, branch string) error {
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return err
	}
	if snap.Branches[branch] == nil {
		return fmt.Errorf("%s: no such local branch", branch)
	}
	if err := r.release(ctx, snap, branch); err != nil {
		return err
	}
	_, err = gitx.Run(ctx, r.Main, "config", "--local", "branch."+branch+"."+parkedKey, "true")
	return err
}

// Unpark removes the parked mark. The branch stays wherever it is.
func (r *Repo) Unpark(ctx context.Context, branch string) error {
	_, err := gitx.Run(ctx, r.Main, "config", "--local", "--unset", "branch."+branch+"."+parkedKey)
	if err != nil && gitx.ExitCode(err) == 5 {
		return nil // was not parked
	}
	return err
}

// Safety summarizes what deleting a branch would lose.
type Safety struct {
	Unpushed    int  // commits not on any origin branch
	OnRemote    bool // origin has a branch of the same name
	MergedIntoB bool // tip is reachable from the base ref
}

// BranchSafety inspects branch before deletion.
func (r *Repo) BranchSafety(ctx context.Context, branch string) Safety {
	var s Safety
	if out, err := gitx.Run(ctx, r.Main, "rev-list", "--count", "refs/heads/"+branch, "--not", "--remotes=origin"); err == nil {
		s.Unpushed, _ = strconv.Atoi(out)
	}
	s.OnRemote = r.refExists(ctx, "refs/remotes/origin/"+branch)
	if r.refExists(ctx, r.Base) {
		_, err := gitx.Run(ctx, r.Main, "merge-base", "--is-ancestor", "refs/heads/"+branch, r.Base)
		s.MergedIntoB = err == nil
	}
	return s
}

// Delete frees the slot holding branch (if any) and deletes the local branch.
// The worktree must be clean; commits not on origin are lost.
func (r *Repo) Delete(ctx context.Context, branch string) error {
	snap, err := r.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err := r.release(ctx, snap, branch); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, r.Main, "branch", "-D", branch); err != nil {
		return fmt.Errorf("delete %s: %s", branch, gitx.Message(err))
	}
	return nil
}

// discard throws away every uncommitted change in a worktree, tracked and
// untracked (ignored files are kept).
func discard(ctx context.Context, path string) error {
	if _, err := gitx.Run(ctx, path, "reset", "--hard", "HEAD"); err != nil {
		return err
	}
	_, err := gitx.Run(ctx, path, "clean", "-fd")
	return err
}
