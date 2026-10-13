package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/talmage89/dv/internal/config"
	"github.com/talmage89/dv/internal/pool"
	"github.com/talmage89/dv/internal/testrepo"
)

func newTestApp(t testing.TB) *App {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := pool.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err := newApp(ctx, cfg, repo, Options{Version: "test", CanCD: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.cancel)
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	// Fill statuses synchronously instead of streaming.
	for _, s := range a.snap.All() {
		a.statuses[s.Path] = pool.StatusOf(ctx, s.Path)
		a.fresh[s.Path] = true
	}
	return a
}

func press(a *App, keys ...string) tea.Cmd {
	var last tea.Cmd
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		default:
			r := []rune(k)[0]
			msg = tea.KeyPressMsg{Code: r, Text: k}
		}
		_, last = a.Update(msg)
	}
	return last
}

// find runs cmd (expanding batches) and returns the first message of type T.
// Commands are run concurrently with a deadline so timers (cursor blink,
// ticks) cannot stall the test.
func find[T tea.Msg](cmd tea.Cmd) (T, bool) {
	var zero T
	if cmd == nil {
		return zero, false
	}
	out := make(chan tea.Msg, 1)
	go func() { out <- cmd() }()
	select {
	case msg := <-out:
		if m, ok := msg.(T); ok {
			return m, true
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				if m, ok := find[T](c); ok {
					return m, true
				}
			}
		}
	case <-time.After(5 * time.Second):
	}
	return zero, false
}

func typeText(a *App, s string) {
	for _, r := range s {
		a.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func render(a *App) string { return a.View().Content }

// Every rendered line must be exactly the terminal width, or the layout
// wraps and the whole screen shifts.
func assertFrame(t *testing.T, a *App) string {
	t.Helper()
	out := render(a)
	lines := strings.Split(out, "\n")
	if len(lines) != a.h {
		t.Errorf("frame has %d lines, want %d", len(lines), a.h)
	}
	for i, l := range lines {
		if w := width(l); w != a.w {
			t.Errorf("line %d is %d wide, want %d: %q", i, w, a.w, stripANSI(l))
		}
		if strings.ContainsRune(l, '\t') {
			t.Errorf("line %d contains a tab: %q", i, stripANSI(l))
		}
	}
	return stripANSI(out)
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			n, _, ok := scanEscape([]byte(s[i:]))
			if ok {
				i += n - 1
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestHomeListsBranchesAndFrameFits(t *testing.T) {
	r := testrepo.New(t, 3)
	r.Checkout(1, "me/first")
	r.Checkout(2, "someone/else")
	a := newTestApp(t)
	out := assertFrame(t, a)
	for _, want := range []string{"me/first", "someone/else", "2 checked out", "1 free"} {
		if !strings.Contains(out, want) {
			t.Errorf("home is missing %q:\n%s", want, out)
		}
	}
}

func TestEveryScreenFits(t *testing.T) {
	r := testrepo.New(t, 2)
	r.Checkout(1, "me/a-very-long-branch-name-that-goes-on-and-on-and-on-past-the-edge-of-the-screen")
	a := newTestApp(t)
	for _, size := range [][2]int{{100, 30}, {60, 16}, {200, 50}} {
		a.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, key := range []string{"N", "S", "O", "R", "C", "L", "?"} {
			press(a, key)
			assertFrame(t, a)
			press(a, "esc")
		}
		assertFrame(t, a)
	}
}

func TestShiftLettersAreCommandsLowercaseIsSearch(t *testing.T) {
	testrepo.New(t, 1)
	a := newTestApp(t)
	typeText(a, "np")
	if len(a.stack) != 1 {
		t.Fatal("lowercase letters must not open screens")
	}
	if got := a.stack[0].(*homeScreen).input.Value(); got != "np" {
		t.Fatalf("search = %q", got)
	}
	press(a, "N")
	if _, ok := a.top().(*newBranchScreen); !ok {
		t.Fatalf("N opened %T", a.top())
	}
	// The search text seeds the new branch name.
	if got := a.top().(*newBranchScreen).name.Value(); got != "np" {
		t.Fatalf("seed = %q", got)
	}
}

func TestPasteNeverTriggersCommands(t *testing.T) {
	testrepo.New(t, 1)
	a := newTestApp(t)
	a.Update(tea.PasteMsg{Content: "  me/Some-Branch-NP\n"})
	if len(a.stack) != 1 {
		t.Fatal("paste opened a screen")
	}
	if got := a.stack[0].(*homeScreen).input.Value(); got != "me/Some-Branch-NP" {
		t.Fatalf("search = %q", got)
	}
}

func TestEnterOnCheckedOutBranchOpensIt(t *testing.T) {
	r := testrepo.New(t, 2)
	r.Checkout(2, "me/target")
	a := newTestApp(t)
	a.Update(tea.PasteMsg{Content: "me/target"})
	cmd := press(a, "enter")
	if a.cdTarget != r.Slot(2) {
		t.Fatalf("cd = %q, want %q", a.cdTarget, r.Slot(2))
	}
	if cmd == nil {
		t.Fatal("expected quit")
	}
}

func TestEnterChecksOutIdleBranch(t *testing.T) {
	r := testrepo.New(t, 2)
	r.Git(r.Main, "branch", "me/idle", "origin/main")
	a := newTestApp(t)
	typeText(a, "idle")
	cmd := press(a, "enter")
	if a.busy == "" || cmd == nil {
		t.Fatal("expected a checkout operation to start")
	}
	done, ok := find[opDoneMsg](cmd)
	if !ok {
		t.Fatal("checkout never finished")
	}
	a.Update(done)
	if a.cdTarget != r.Slot(1) {
		t.Fatalf("cd = %q, want dev-1", a.cdTarget)
	}
}

func TestRemoteRowOffersFetch(t *testing.T) {
	testrepo.New(t, 1)
	a := newTestApp(t)
	typeText(a, "teammate/new-thing")
	out := assertFrame(t, a)
	if !strings.Contains(out, "fetch teammate/new-thing from origin") {
		t.Fatalf("missing remote row:\n%s", out)
	}
}

func TestParkRefusesDirtyWorktreeInUI(t *testing.T) {
	r := testrepo.New(t, 1)
	r.Checkout(1, "me/spike")
	r.Write(r.Slot(1), "test.md", "")
	a := newTestApp(t)
	press(a, "Z")
	if a.busy != "" {
		t.Fatal("park started despite uncommitted changes")
	}
	if a.toast == nil || !strings.Contains(a.toast.text, "uncommitted") {
		t.Fatalf("toast = %+v", a.toast)
	}
}

func TestOutBuf(t *testing.T) {
	var o outBuf
	o.write([]byte("progress 10%\rprogress 100%\nred \x1b[31mtext\x1b[0m\x1b[2K done\r\n"))
	o.write([]byte("split \x1b[3"))
	o.write([]byte("2mgreen\x1b[0m"))
	got := o.all()
	want := []string{"progress 100%", "red \x1b[31mtext\x1b[0m done", "split \x1b[32mgreen\x1b[0m"}
	if len(got) != len(want) {
		t.Fatalf("lines = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestValidRefName(t *testing.T) {
	for _, ok := range []string{"me/feature", "bot-123-x", "a.b"} {
		if !validRefName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-x", "a..b", "a b", "a:b", "x.lock", "a/", "a//b", "a?", "@", "a/.b"} {
		if validRefName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestHistoryRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	saveHistory(nil, "git status")
	saveHistory(loadHistory(), "npm test")
	saveHistory(loadHistory(), "git status")
	got := loadHistory()
	if len(got) != 2 || got[0] != "npm test" || got[1] != "git status" {
		t.Fatalf("history = %q", got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_STATE_HOME"), "dv", "run_history")); err != nil {
		t.Fatal(err)
	}
}
