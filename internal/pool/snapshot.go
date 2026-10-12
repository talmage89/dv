package pool

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/talmage89/dv/internal/gitx"
)

// Slot is one worktree: a pooled slot, the main worktree, or a worktree that
// lives outside the pool root ("external").
type Slot struct {
	Path     string
	Name     string // directory name
	Num      int    // slot number; -1 for main and external worktrees
	Branch   string // empty when detached
	Head     string
	Locked   bool
	Missing  bool // registered with git but the directory is gone
	Main     bool
	External bool
}

// Detached reports whether the slot has no branch checked out.
func (s Slot) Detached() bool { return s.Branch == "" }

// Branch is a local branch with the metadata the UI shows.
type Branch struct {
	Name       string
	Upstream   string
	Gone       bool // upstream configured but deleted on the remote
	Ahead      int  // commits ahead of upstream
	Behind     int  // commits behind upstream
	BaseAhead  int  // commits ahead of the base ref (origin/main)
	BaseBehind int
	HasBase    bool // BaseAhead/BaseBehind are meaningful
	CommitTime time.Time
	Subject    string
	Parked     bool
	Worktree   string    // path of the worktree holding it, if checked out
	LastActive time.Time // best estimate of when it was last worked on
}

// Snapshot is the pool's state at one moment, built from cheap local reads
// only (no network, no `git status`), so it is fast enough to take before the
// first frame.
type Snapshot struct {
	Main     Slot
	Slots    []Slot // pool slots, sorted by number
	External []Slot // worktrees outside the pool root
	Branches map[string]*Branch
	TakenAt  time.Time

	// Memoized sorted views; a snapshot is immutable once built.
	checkedOut, parked, idle, gone []*Branch
	memo                           sync.Once
}

func (s *Snapshot) sorted() {
	s.memo.Do(func() {
		for _, sl := range append(append([]Slot{}, s.Slots...), s.External...) {
			if b := s.Branches[sl.Branch]; b != nil {
				s.checkedOut = append(s.checkedOut, b)
			}
		}
		for _, b := range s.Branches {
			switch {
			case b.Worktree == "" && b.Parked:
				s.parked = append(s.parked, b)
			case b.Worktree == "":
				s.idle = append(s.idle, b)
			}
			if b.Gone {
				s.gone = append(s.gone, b)
			}
		}
		for _, l := range [][]*Branch{s.checkedOut, s.parked, s.idle, s.gone} {
			sortByActivity(l)
		}
	})
}

// Snapshot reads the current state of the pool. The independent git reads
// run concurrently; the whole call is a few tens of milliseconds on a repo
// with hundreds of branches.
func (r *Repo) Snapshot(ctx context.Context) (*Snapshot, error) {
	var (
		wtOut, refOut, parkedOut string
		wtErr, refErr            error
		hasBase                  bool
		wg                       sync.WaitGroup
	)
	wg.Add(4)
	go func() { defer wg.Done(); wtOut, wtErr = gitx.Run(ctx, r.Main, "worktree", "list", "--porcelain") }()
	go func() {
		defer wg.Done()
		refOut, refErr = gitx.Run(ctx, r.Main, "for-each-ref", "--format="+branchFormat, "refs/heads")
	}()
	go func() {
		defer wg.Done()
		parkedOut, _ = gitx.Run(ctx, r.Main, "config", "--local", "--get-regexp", `^branch\..*\.`+parkedKey+`$`)
	}()
	go func() { defer wg.Done(); hasBase = r.refExists(ctx, r.Base) }()
	wg.Wait()
	if wtErr != nil {
		return nil, wtErr
	}
	if refErr != nil {
		return nil, refErr
	}

	snap := &Snapshot{Branches: map[string]*Branch{}, TakenAt: time.Now()}
	for i, wt := range gitx.ParseWorktrees(wtOut) {
		s := Slot{
			Path:    wt.Path,
			Name:    baseName(wt.Path),
			Num:     -1,
			Branch:  wt.Branch,
			Head:    wt.Head,
			Locked:  wt.Locked,
			Missing: wt.Prunable,
		}
		switch {
		case i == 0:
			s.Main = true
			snap.Main = s
		case r.slotNum(wt.Path) >= 0:
			s.Num = r.slotNum(wt.Path)
			snap.Slots = append(snap.Slots, s)
		default:
			s.External = true
			snap.External = append(snap.External, s)
		}
	}
	sort.Slice(snap.Slots, func(i, j int) bool { return snap.Slots[i].Num < snap.Slots[j].Num })

	parseBranches(snap, refOut)
	parseParked(snap, parkedOut)
	for _, s := range snap.All() {
		if b := snap.Branches[s.Branch]; b != nil {
			b.Worktree = s.Path
		}
	}
	if hasBase {
		r.readBasePositions(ctx, snap)
	}
	r.fillActivity(snap)
	return snap, nil
}

// All returns main, then pool slots, then external worktrees.
func (s *Snapshot) All() []Slot {
	all := make([]Slot, 0, 1+len(s.Slots)+len(s.External))
	all = append(all, s.Main)
	all = append(all, s.Slots...)
	return append(all, s.External...)
}

// SlotFor returns the worktree holding branch.
func (s *Snapshot) SlotFor(branch string) (Slot, bool) {
	for _, sl := range s.All() {
		if sl.Branch == branch && branch != "" {
			return sl, true
		}
	}
	return Slot{}, false
}

// SlotAt returns the worktree at path.
func (s *Snapshot) SlotAt(path string) (Slot, bool) {
	for _, sl := range s.All() {
		if sl.Path == path {
			return sl, true
		}
	}
	return Slot{}, false
}

// CheckedOut returns branches held by pool slots and external worktrees (not
// main), most recently active first.
func (s *Snapshot) CheckedOut() []*Branch { s.sorted(); return s.checkedOut }

// Parked returns parked branches that are not checked out, most recently
// active first.
func (s *Snapshot) Parked() []*Branch { s.sorted(); return s.parked }

// Idle returns local branches that are neither checked out nor parked, most
// recently active first.
func (s *Snapshot) Idle() []*Branch { s.sorted(); return s.idle }

// Gone returns local branches whose upstream was deleted on the remote.
func (s *Snapshot) Gone() []*Branch { s.sorted(); return s.gone }

func sortByActivity(bs []*Branch) {
	sort.SliceStable(bs, func(i, j int) bool {
		if !bs[i].LastActive.Equal(bs[j].LastActive) {
			return bs[i].LastActive.After(bs[j].LastActive)
		}
		return bs[i].Name < bs[j].Name
	})
}

var branchFormat = strings.Join([]string{
	"%(refname:short)",
	"%(upstream:short)",
	"%(upstream:track,nobracket)",
	"%(committerdate:unix)",
	"%(contents:subject)",
}, "%00")

func parseBranches(snap *Snapshot, out string) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x00")
		if len(f) < 5 {
			continue
		}
		b := &Branch{Name: f[0], Upstream: f[1], Subject: f[4]}
		parseTrack(b, f[2])
		if ts, err := strconv.ParseInt(f[3], 10, 64); err == nil {
			b.CommitTime = time.Unix(ts, 0)
		}
		snap.Branches[b.Name] = b
	}
}

// readBasePositions fills ahead/behind the base ref for the branches the UI
// lists by default (checked out or parked). Computing it for every branch
// doubles the cost of a snapshot on repos with hundreds of branches.
func (r *Repo) readBasePositions(ctx context.Context, snap *Snapshot) {
	args := []string{"for-each-ref", "--format=%(refname:short)%00%(ahead-behind:" + r.Base + ")"}
	for _, b := range snap.Branches {
		if b.Worktree != "" || b.Parked {
			args = append(args, "refs/heads/"+b.Name)
		}
	}
	if len(args) == 2 {
		return
	}
	out, err := gitx.Run(ctx, r.Main, args...)
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		name, ab, _ := strings.Cut(line, "\x00")
		a, bh, ok := strings.Cut(ab, " ")
		if b := snap.Branches[name]; b != nil && ok {
			b.BaseAhead, _ = strconv.Atoi(a)
			b.BaseBehind, _ = strconv.Atoi(bh)
			b.HasBase = true
		}
	}
}

// parseTrack parses "%(upstream:track,nobracket)": "gone", "ahead 2",
// "behind 1", "ahead 2, behind 1", or empty.
func parseTrack(b *Branch, track string) {
	if track == "gone" {
		b.Gone = true
		return
	}
	for _, part := range strings.Split(track, ", ") {
		kind, n, ok := strings.Cut(part, " ")
		if !ok {
			continue
		}
		v, _ := strconv.Atoi(n)
		switch kind {
		case "ahead":
			b.Ahead = v
		case "behind":
			b.Behind = v
		}
	}
}

// parkedKey is the per-branch git config variable marking a parked branch.
// It lives in the branch's own config section, so git removes it when the
// branch is deleted or renamed along with the rest of branch.<name>.*.
const parkedKey = "dvparked"

func parseParked(snap *Snapshot, out string) {
	for _, line := range strings.Split(out, "\n") {
		key, val, _ := strings.Cut(line, " ")
		name := strings.TrimSuffix(strings.TrimPrefix(key, "branch."), "."+parkedKey)
		if b := snap.Branches[name]; b != nil && val == "true" {
			b.Parked = true
		}
	}
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
