package pool

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/talmage89/dv/internal/gitx"
)

// MaxSlots caps the pool so a stray keypress cannot create hundreds of
// worktrees.
const MaxSlots = 64

// ScalePlan is what changing the pool size to a target would do.
type ScalePlan struct {
	Current   int
	Target    int
	Add       []string // slot paths to create
	Remove    []Slot   // free slots to delete, highest-numbered first
	Removable int      // how many slots could be removed at most
	Shortfall int      // requested removals that cannot happen (slots in use)
}

// PlanScale computes the plan for resizing the pool to target slots. Only
// free slots (detached, clean, no Claude session) are ever removed; statuses
// and live come from the caller's latest reads.
func (r *Repo) PlanScale(snap *Snapshot, target int, statuses map[string]Status, live map[string]int) ScalePlan {
	target = min(max(target, 0), MaxSlots)
	p := ScalePlan{Current: len(snap.Slots), Target: target}
	var free []Slot
	for _, s := range snap.Slots {
		st, known := statuses[s.Path]
		if s.Missing || (s.Detached() && !s.Locked && known && st.Known() && st.Err == "" && !st.Dirty && live[s.Path] == 0) {
			free = append(free, s)
		}
	}
	sort.Slice(free, func(i, j int) bool {
		// Missing directories go first: removing them is pure bookkeeping.
		if free[i].Missing != free[j].Missing {
			return free[i].Missing
		}
		return free[i].Num > free[j].Num
	})
	p.Removable = len(free)
	switch {
	case target > p.Current:
		used := map[int]bool{}
		for _, s := range snap.Slots {
			used[s.Num] = true
		}
		for n := 1; len(p.Add) < target-p.Current; n++ {
			if used[n] {
				continue
			}
			if _, err := os.Stat(r.SlotPath(n)); err == nil {
				continue // a stray directory already has this name
			}
			p.Add = append(p.Add, r.SlotPath(n))
		}
	case target < p.Current:
		want := p.Current - target
		n := min(want, len(free))
		p.Remove = free[:n]
		p.Shortfall = want - n
	}
	return p
}

// PrepareAdd fetches the base ref once before creating slots.
func (r *Repo) PrepareAdd(ctx context.Context) error {
	if remote, rb, ok := strings.Cut(r.Base, "/"); ok && r.isRemote(ctx, remote) {
		if _, err := gitx.Run(ctx, r.Main, "fetch", remote, rb); err != nil {
			return fmt.Errorf("fetch %s: %s", r.Base, gitx.Message(err))
		}
	}
	return nil
}

// AddSlot creates a detached worktree at path, at the base ref.
func (r *Repo) AddSlot(ctx context.Context, path string) error {
	target := r.Base
	if !r.refExists(ctx, target) {
		target = "HEAD"
	}
	if err := os.MkdirAll(r.Root, 0o755); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, r.Main, "worktree", "add", "--detach", path, target); err != nil {
		return fmt.Errorf("create %s: %s", baseName(path), gitx.Message(err))
	}
	return nil
}

// RemoveSlot deletes a free slot. git refuses if the worktree gained changes
// since it was judged free, so nothing unsaved is ever lost.
func (r *Repo) RemoveSlot(ctx context.Context, s Slot) error {
	if s.Missing {
		_, err := gitx.Run(ctx, r.Main, "worktree", "prune")
		return err
	}
	if !s.Detached() {
		return fmt.Errorf("%s holds %s", s.Name, s.Branch)
	}
	if _, err := gitx.Run(ctx, r.Main, "worktree", "remove", s.Path); err != nil {
		return fmt.Errorf("remove %s: %s", s.Name, gitx.Message(err))
	}
	return nil
}
