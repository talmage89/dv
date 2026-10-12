package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talmage89/dv/internal/testrepo"
)

// silence discards stdout/stderr for the duration of a test.
func silence(t *testing.T) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devnull, devnull
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		devnull.Close()
	})
}

func cdFile(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "cd")
	t.Setenv("DV_CD_FILE", f)
	return f
}

func readCD(t *testing.T, f string) string {
	t.Helper()
	data, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	return string(data)
}

func TestBranchArgumentChecksOutAndHandsOffDirectory(t *testing.T) {
	r := testrepo.New(t, 2)
	r.Git(r.Main, "branch", "me/feature", "origin/main")
	f := cdFile(t)
	silence(t)
	if code := Main(context.Background(), []string{"me/feature"}, "test"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := readCD(t, f); got != r.Slot(1) {
		t.Fatalf("cd = %q, want %q", got, r.Slot(1))
	}
}

func TestMainAndNew(t *testing.T) {
	r := testrepo.New(t, 1)
	f := cdFile(t)
	silence(t)
	if code := Main(context.Background(), []string{"main"}, "test"); code != 0 || readCD(t, f) != r.Main {
		t.Fatalf("dv main: exit %d, cd %q", code, readCD(t, f))
	}
	if code := Main(context.Background(), []string{"new", "me/fresh"}, "test"); code != 0 || readCD(t, f) != r.Slot(1) {
		t.Fatalf("dv new: exit %d, cd %q", code, readCD(t, f))
	}
}

func TestRemovedCommandsExplainThemselves(t *testing.T) {
	testrepo.New(t, 1)
	silence(t)
	for _, args := range [][]string{{"tidy"}, {"init", "3"}, {"3"}, {"branches"}} {
		if code := Main(context.Background(), args, "test"); code != 2 {
			t.Errorf("dv %s: exit %d, want 2", strings.Join(args, " "), code)
		}
	}
}

// Shells opened before the upgrade still have the old wrapper, which calls
// `dv __resolve_path` and treats exit 99 as "run me normally".
func TestLegacyResolvePath(t *testing.T) {
	r := testrepo.New(t, 1)
	t.Setenv("DV_CD_FILE", "")
	silence(t)
	for _, sub := range []string{"run", "prune", "config", "ls", "tidy", "--help", "sync"} {
		if code := Main(context.Background(), []string{"__resolve_path", sub}, "test"); code != exitLegacyPassthrough {
			t.Errorf("__resolve_path %s: exit %d, want %d", sub, code, exitLegacyPassthrough)
		}
	}
	// A cd target prints the directory on stdout.
	out := captureStdout(t, func() {
		if code := Main(context.Background(), []string{"__resolve_path", "main"}, "test"); code != 0 {
			t.Errorf("exit %d", code)
		}
	})
	if strings.TrimSpace(out) != r.Main {
		t.Fatalf("stdout = %q, want %q", out, r.Main)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = wr
	fn()
	os.Stdout = old
	wr.Close()
	data, _ := io.ReadAll(rd)
	return string(data)
}

func TestShellInitParses(t *testing.T) {
	for _, sh := range []string{"bash", "zsh"} {
		if _, err := exec.LookPath(sh); err != nil {
			continue
		}
		if out, err := exec.Command(sh, "-n", "-c", shellInit).CombinedOutput(); err != nil {
			t.Errorf("%s rejects shell-init: %v\n%s", sh, err, out)
		}
	}
}

func TestReorderFlags(t *testing.T) {
	got := reorder([]string{"me/x", "--base", "origin/dev"})
	want := []string{"--base", "origin/dev", "me/x"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("reorder = %q", got)
	}
}
