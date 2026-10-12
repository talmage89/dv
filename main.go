// Command dv manages a pool of git worktrees. Run it with no arguments for the
// full-screen interface; subcommands cover scripting and muscle memory.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/talmage89/dv/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], resolveVersion())
	stop()
	os.Exit(code)
}

func resolveVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	// A tagged build reports its tag. Untagged builds report a pseudo-version
	// (v1.0.1-0.2026…-abcdef123456), which is noise; fall through to the
	// short commit instead.
	if v := info.Main.Version; v != "" && v != "(devel)" && strings.Count(v, "-") < 2 {
		return v
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if rev == "" {
		return "dev"
	}
	if dirty {
		rev += "+dirty"
	}
	return fmt.Sprintf("dev-%s", rev)
}
