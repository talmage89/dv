package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"DEV_REPO", "DEV_ROOT", "DEV_USER"} {
		t.Setenv(k, "")
	}
	t.Setenv("USER", "fallback")
	return dir
}

func TestPrecedence(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	c, err := Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v := c.Get(KeyUser); v.Value != "fallback" || v.Source != SourceDefault {
		t.Fatalf("default user = %+v", v)
	}
	if err := Set(ctx, KeyUser, "stored"); err != nil {
		t.Fatal(err)
	}
	c, _ = Load(ctx)
	if v := c.Get(KeyUser); v.Value != "stored" || v.Source != SourceGit {
		t.Fatalf("git user = %+v", v)
	}
	t.Setenv("DEV_USER", "fromenv")
	c, _ = Load(ctx)
	if v := c.Get(KeyUser); v.Value != "fromenv" || v.Source != SourceEnv {
		t.Fatalf("env user = %+v", v)
	}
	if err := Unset(ctx, KeyUser); err != nil {
		t.Fatal(err)
	}
	if err := Unset(ctx, KeyUser); err != nil {
		t.Fatalf("unsetting twice: %v", err)
	}
}

func TestSetValidates(t *testing.T) {
	dir := isolate(t)
	ctx := context.Background()
	if err := Set(ctx, KeySyncEnv, "maybe"); err == nil {
		t.Error("accepted a non-boolean")
	}
	if err := Set(ctx, KeyParallelism, "0"); err == nil {
		t.Error("accepted zero parallelism")
	}
	if err := Set(ctx, KeyRepo, dir); err == nil {
		t.Error("accepted a repo path that is not a git worktree")
	}
	if err := Set(ctx, "nope", "x"); err == nil {
		t.Error("accepted an unknown key")
	}
	if err := Set(ctx, KeySyncEnv, "FALSE"); err != nil {
		t.Fatal(err)
	}
	c, _ := Load(ctx)
	if c.Bool(KeySyncEnv) {
		t.Error("syncEnv should be off")
	}
}

func TestRootDefaultsBesideMain(t *testing.T) {
	isolate(t)
	c, _ := Load(context.Background())
	c.MainHint = "/work/app"
	if got := c.String(KeyRoot); got != "/work/app-worktrees" {
		t.Fatalf("root = %q", got)
	}
	if got := c.String(KeyEnvSource); got != "/work/app-worktrees/.env" {
		t.Fatalf("env source = %q", got)
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := expandHome("~/x"); got != filepath.Join(home, "x") {
		t.Fatal(got)
	}
}
