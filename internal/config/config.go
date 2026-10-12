// Package config resolves dv's settings. Each setting comes from, in order:
// an environment variable (when it has one), `git config --global dev.<key>`,
// or a built-in default. Values are persisted with `git config --global`, the
// same store the bash version of dv used, so existing settings carry over.
package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/talmage89/dv/internal/gitx"
)

// Source says where a resolved value came from.
type Source int

const (
	SourceDefault Source = iota
	SourceGit
	SourceEnv
	SourceDerived // computed from other settings or the environment ($PWD)
)

// Key identifies a setting.
type Key string

const (
	KeyRepo        Key = "repo"
	KeyRoot        Key = "root"
	KeyUser        Key = "user"
	KeyPrefix      Key = "prefix"
	KeyBase        Key = "base"
	KeyEnvSource   Key = "envSource"
	KeyEnvLine     Key = "envLine"
	KeySyncEnv     Key = "syncEnv"
	KeySyncClaude  Key = "syncClaude"
	KeyParallelism Key = "runJobs"
	KeyPalette     Key = "palette"
	KeyEmails      Key = "emails"
)

// Spec describes a setting for the config screen and `dv config`.
type Spec struct {
	Key     Key
	Label   string
	Help    string
	EnvVar  string
	Bool    bool
	Int     bool
	Path    bool
	Options []string // allowed values, for a choice setting
	Default func(c *Config) string
}

// Specs lists every setting in display order. It is assigned in init because
// some defaults resolve other settings through Get, which reads Specs.
var Specs []Spec

func init() {
	Specs = specs()
}

func specs() []Spec {
	return []Spec{
		{Key: KeyRepo, Label: "Repository", Path: true, EnvVar: "DEV_REPO",
			Help: "Any worktree of the repo dv manages. Falls back to the repo containing $PWD."},
		{Key: KeyRoot, Label: "Worktree root", Path: true, EnvVar: "DEV_ROOT",
			Help: "Directory holding the pooled worktrees. Defaults to <repo>-worktrees beside the main worktree.",
			Default: func(c *Config) string {
				if c.MainHint != "" {
					return c.MainHint + "-worktrees"
				}
				return ""
			}},
		{Key: KeyUser, Label: "Branch namespace", EnvVar: "DEV_USER",
			Help:    "Your branch prefix (user/…), suggested when a new branch name has none.",
			Default: func(*Config) string { return os.Getenv("USER") }},
		{Key: KeyPrefix, Label: "Worktree prefix",
			Help:    "Pooled worktrees are named <prefix><n> inside the root.",
			Default: func(*Config) string { return "dev-" }},
		{Key: KeyBase, Label: "New branch base",
			Help: "Ref new branches start from. Fetched before each new branch. Defaults to origin's default branch.",
			Default: func(c *Config) string {
				if ref, err := gitx.Run(context.Background(), c.MainHint, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
					return ref
				}
				return "origin/main"
			}},
		{Key: KeyEmails, Label: "Your emails",
			Help: "Comma-separated commit author emails that make a branch yours. dv sweep frees worktrees holding branches without a commit by any of them.",
			Default: func(c *Config) string {
				email, _ := gitx.Run(context.Background(), c.MainHint, "config", "user.email")
				return email
			}},
		{Key: KeySyncEnv, Label: "Sync .env", Bool: true,
			Help:    "Keep each worktree's .env equal to the shared source plus its own line.",
			Default: func(*Config) string { return "false" }},
		{Key: KeyEnvSource, Label: ".env source", Path: true,
			Help: "Shared .env copied into every worktree.",
			Default: func(c *Config) string {
				if root := c.Get(KeyRoot).Value; root != "" {
					return filepath.Join(root, ".env")
				}
				return ""
			}},
		{Key: KeyEnvLine, Label: ".env extra line",
			Help: "Appended to each worktree's .env, e.g. a per-worktree database URL. {name} is the worktree directory name."},
		{Key: KeySyncClaude, Label: "Sync .claude", Bool: true,
			Help:    "Symlink the main worktree's gitignored .claude files into every worktree.",
			Default: func(*Config) string { return "true" }},
		{Key: KeyParallelism, Label: "Run parallelism", Int: true,
			Help:    "How many worktrees the run screen executes at once.",
			Default: func(*Config) string { return "4" }},
		{Key: KeyPalette, Label: "Selection color", Options: []string{"tide", "ember", "orchid", "mono"},
			Help:    "The drifting gradient on the selected row: tide (teal to blue), ember (coral to amber), orchid (rose to violet), or mono (no color, no motion).",
			Default: func(*Config) string { return "tide" }},
	}
}

// SpecFor returns the spec for key.
func SpecFor(key Key) (Spec, bool) {
	for _, s := range Specs {
		if s.Key == key {
			return s, true
		}
	}
	return Spec{}, false
}

// Value is a resolved setting.
type Value struct {
	Value  string
	Source Source
	EnvVar string // set when Source == SourceEnv
}

// SourceLabel is a short human description of where the value came from.
func (v Value) SourceLabel() string {
	switch v.Source {
	case SourceEnv:
		return "$" + v.EnvVar
	case SourceGit:
		return "git config"
	case SourceDerived:
		return "detected"
	default:
		return "default"
	}
}

// Config is a snapshot of every setting.
type Config struct {
	values map[Key]Value
	// MainHint is the main worktree path, filled in by the caller once the
	// repo resolves, so path defaults can derive from it.
	MainHint string
	pwd      string
}

// Load reads every setting from the environment and global git config.
func Load(ctx context.Context) (*Config, error) {
	c := &Config{values: map[Key]Value{}}
	c.pwd, _ = os.Getwd()
	stored, err := readGitConfig(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range Specs {
		if s.EnvVar != "" {
			if v := os.Getenv(s.EnvVar); v != "" {
				c.values[s.Key] = Value{Value: v, Source: SourceEnv, EnvVar: s.EnvVar}
				continue
			}
		}
		if v, ok := stored[strings.ToLower(string(s.Key))]; ok && v != "" {
			c.values[s.Key] = Value{Value: v, Source: SourceGit}
		}
	}
	if _, ok := c.values[KeyRepo]; !ok && c.pwd != "" {
		if top, err := gitx.Run(ctx, c.pwd, "rev-parse", "--show-toplevel"); err == nil {
			c.values[KeyRepo] = Value{Value: top, Source: SourceDerived}
		}
	}
	return c, nil
}

// readGitConfig returns every dev.* key from the global git config. Git
// lowercases variable names in --get-regexp output, so keys are lowercased.
func readGitConfig(ctx context.Context) (map[string]string, error) {
	out, err := gitx.Run(ctx, "", "config", "--global", "--get-regexp", `^dev\.`)
	if err != nil {
		// Exit 1 means no matching keys, and a missing ~/.gitconfig is fine.
		if gitx.ExitCode(err) == 1 {
			return map[string]string{}, nil
		}
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(line, " ")
		if k = strings.TrimPrefix(k, "dev."); k != "" {
			m[strings.ToLower(k)] = v
		}
	}
	return m, nil
}

// Get resolves key, falling back to its default.
func (c *Config) Get(key Key) Value {
	if v, ok := c.values[key]; ok {
		return v
	}
	spec, _ := SpecFor(key)
	if spec.Default != nil {
		return Value{Value: spec.Default(c), Source: SourceDefault}
	}
	return Value{Source: SourceDefault}
}

// String is shorthand for Get(key).Value.
func (c *Config) String(key Key) string { return c.Get(key).Value }

// Bool parses a boolean setting; unparseable values count as the default.
func (c *Config) Bool(key Key) bool {
	b, err := strconv.ParseBool(c.String(key))
	if err != nil {
		spec, _ := SpecFor(key)
		b, _ = strconv.ParseBool(spec.Default(c))
	}
	return b
}

// Int parses an integer setting, clamped to at least 1.
func (c *Config) Int(key Key) int {
	n, err := strconv.Atoi(c.String(key))
	if err != nil || n < 1 {
		spec, _ := SpecFor(key)
		n, _ = strconv.Atoi(spec.Default(c))
	}
	return max(n, 1)
}

// Set validates and persists value for key in the global git config.
func Set(ctx context.Context, key Key, value string) error {
	spec, ok := SpecFor(key)
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	value = strings.TrimSpace(value)
	switch {
	case spec.Bool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("%s must be true or false", key)
		}
		value = strconv.FormatBool(b)
	case spec.Int:
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return fmt.Errorf("%s must be a positive number", key)
		}
	case len(spec.Options) > 0:
		if !slices.Contains(spec.Options, value) {
			return fmt.Errorf("%s must be one of %s", key, strings.Join(spec.Options, ", "))
		}
	case spec.Path:
		value = expandHome(value)
		abs, err := filepath.Abs(value)
		if err != nil {
			return err
		}
		value = abs
	}
	if key == KeyRepo {
		if _, err := gitx.Run(ctx, value, "rev-parse", "--is-inside-work-tree"); err != nil {
			return fmt.Errorf("%s is not inside a git worktree", value)
		}
	}
	_, err := gitx.Run(ctx, "", "config", "--global", "dev."+string(key), value)
	return err
}

// Unset removes key from the global git config. Unsetting a key that is not
// set is not an error.
func Unset(ctx context.Context, key Key) error {
	if _, ok := SpecFor(key); !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	_, err := gitx.Run(ctx, "", "config", "--global", "--unset", "dev."+string(key))
	if err != nil && gitx.ExitCode(err) == 5 {
		return nil
	}
	return err
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
