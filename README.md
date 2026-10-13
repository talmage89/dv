# dv

A full-screen manager for a pool of git worktrees.

Keeping several long-lived branches checked out side by side avoids the cost
of switching branches in one clone (reinstalling dependencies, restarting dev
servers, per-branch databases). dv treats those worktrees as an anonymous
pool: you ask for a branch, and dv finds it or checks it out into a free
worktree. Directory names never matter, so worktrees are never repacked and
a long-running session stays in its directory.

```
  dv  my-project

  / search, or paste a branch and press enter

  ›   1m  ◆ alex/checkout-retry-backoff
     53m  ◆ alex/settings-page-redesign
     21h *  alex/flaky-upload-test
      2d    alex/faster-search-index

            parked
     13h    alex/2026-09-11-orm-upgrade-spike

            fix: retry checkout on a transient gateway error  3h
            clean · 1 Claude session · pushed · 8 ahead, 0 behind main · active 10:29

  10 checked out · 9 free · 1 parked
  enter open   N new   W fresh   Z park   S sweep   R run   O scale   M main   C config   ? help
```

## Install

Requires git 2.41+. Releases include binaries for macOS and Linux on amd64
and arm64. This installs the latest one to `~/.local/bin` (which must be on
your `PATH`); run it again to upgrade:

```bash
os=$(uname -s | tr A-Z a-z); arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
mkdir -p ~/.local/bin
curl -fsSL "https://github.com/talmage89/dv/releases/latest/download/dv_${os}_${arch}.tar.gz" \
  | tar -xz -C ~/.local/bin dv
dv install --write    # adds eval "$(command dv shell-init)" to your shell rc
```

A binary downloaded through a browser on macOS is quarantined; clear it with
`xattr -d com.apple.quarantine dv`. The
[releases page](https://github.com/talmage89/dv/releases) lists every version
with checksums.

To build from source instead (Go 1.25+), which is also how custom
[key bindings](#key-bindings) are compiled in:

```bash
git clone https://github.com/talmage89/dv.git
cd dv
make install          # builds ~/.local/bin/dv (PREFIX=… to change)
dv install --write
```

The shell integration is what lets dv change your shell's directory. A
program cannot do that itself, so dv writes the target to a file and the
`dv` shell function changes into it.

Run `dv` from inside your clone and it manages that repository. Run it
anywhere else and it asks which clone to manage, then remembers the answer.
Your clone can live anywhere; pooled worktrees go in a directory beside it:

```
my-project/              your clone (the main worktree)
my-project-worktrees/    the pool (the root setting)
  dev-1/                 created with O in the interface, or dv scale
  dev-2/
```

## The interface

`dv` with no arguments opens it. Typing always goes into the search box, and
Shift+letter runs a command. That split is unambiguous because branch names
are lowercase, and a paste arrives as one event, so pasting a branch name
never triggers a command.

The list shows checked-out branches, most recently worked on first, then
parked branches. While you search, other local branches are listed too, and
a name that does not exist locally is offered as a fetch from origin.

| Key | |
|---|---|
| enter | open the selected branch, checking it out into a free worktree if needed |
| N | new branch from the base ref (prefilled with the search text) |
| W | fresh worktree: a free one, detached at the base ref |
| Z | park / unpark: free the worktree but keep the branch listed |
| D | detach: free the worktree without parking |
| X | delete the branch |
| S | sweep: delete branches deleted on origin, and free worktrees holding branches with no commit of yours |
| R | run a command in every worktree, with per-worktree output |
| O | scale the number of pooled worktrees |
| M | go to the main worktree |
| C | config |
| L | log and sync issues |
| F | fetch origin |
| Y | copy the branch name |
| ? | every key for the current screen |

Row markers: red `*` means uncommitted changes, `◆` means a Claude Code
session is running in that worktree, and `gone` means the upstream was
deleted. The selected row carries a slowly drifting gradient; pick its colors
in config (`tide`, `ember`, `orchid`, or `mono` for none).

### Key bindings

Every key on every screen is defined in
[`keymap/defaults.conf`](keymap/defaults.conf), one action per line. To
change one, copy its line into `keymap/overrides.conf` (gitignored), edit the
keys, and run `make install`. The hints, help sheets, and messages all follow
your bindings. Override lines that name an unknown action, and keys bound to
two actions on one screen, are reported in the log (L).

## Behavior worth knowing

**Recency.** A branch's age is the latest of its commits, its reflog, checkouts
in its worktree, and Claude Code transcript activity in its worktree. The
Claude activity is counted only after the branch was checked out there, so a
previous occupant's sessions are not attributed to it.

**Free worktrees.** A worktree is free when it is detached, has no uncommitted
changes, and has no running Claude session. dv never checks a branch out into
a worktree that is not free, and scaling down only removes free worktrees.

**Parking** requires a clean worktree. The mark lives in the branch's own git
config section (`branch.<name>.dvparked`), so git removes it when the branch
is deleted. Checking a parked branch out unparks it.

**Sweep** clears away two kinds of branch after fetching origin. A branch
whose upstream was deleted on origin, usually after a merge, is deleted along
with its worktree checkout. A checked-out branch that is not yours is freed
from its worktree and kept; it is not yours when none of the commits it has
beyond the base ref is authored by one of your emails (a merged branch is
judged by its last commit). Uncommitted changes, parked branches, and running
Claude sessions are opt-in, and only a deleted branch's changes can be
discarded.

**Sync** runs in the background every time the interface opens, and after
scaling up. With `.env` sync on, it keeps each worktree's `.env` equal to a
shared source plus one line naming the worktree, which suits per-worktree
ports or databases. It also symlinks the main worktree's gitignored `.claude`
files into every worktree. Automatic sync only makes changes that cannot lose
data. A worktree `.env` edited after the shared source, or a regular file
where a link belongs, is reported in the log (L) instead of overwritten; force
sync from the log to overwrite it.

**Startup** draws from local state immediately. Dirty markers come from the
last run's cache and refresh in the background, along with live Claude
sessions and a fetch.

## Command line

```
dv <branch>            go to <branch>, checking it out if needed
dv new <branch>        create <branch> from the base ref and go there
dv main                go to the main worktree
dv ls                  checked-out and parked branches, most recent first
dv park | unpark <branch>
dv sweep [--force]     delete branches deleted on origin, free worktrees holding
                       branches with no commit of yours (--force discards changes)
dv scale [<n>]         show or change the number of pooled worktrees
dv run [-x <dir>]... <command>
dv sync [env|claude] [--force]
dv unsync claude
dv config [<key> [<value> | --unset]]
```

Commands started by `dv run` (and the R screen) see `DV_WORKTREE`, `DV_BRANCH`,
`DV_MAIN` (the main worktree), and `DV_ROOT` (the pool directory).

## Configuration

Every setting is editable in the interface (C) or with `dv config`, and is
stored in your global git config under `[dev]`. `DEV_REPO`, `DEV_ROOT`, and
`DEV_USER` override the stored values for one shell.

| Key | Default | |
|---|---|---|
| `repo` | repo containing `$PWD` | any worktree of the managed repo |
| `root` | `<repo>-worktrees` beside the main worktree | where pooled worktrees live |
| `user` | `$USER` | your branch namespace, suggested for new branches |
| `prefix` | `dev-` | pooled worktree directory prefix |
| `base` | origin's default branch | where new branches start |
| `emails` | git `user.email` | your commit emails, comma-separated; sweep keeps worktrees whose branch has a commit by any of them |
| `syncEnv` | `false` | keep each worktree's `.env` in sync |
| `envSource` | `<root>/.env` | shared `.env` |
| `envLine` | none | appended per worktree; `{name}` is the worktree directory name |
| `syncClaude` | `true` | link the main worktree's gitignored `.claude` files |
| `runJobs` | `4` | parallelism of the run screen |
| `palette` | `tide` | selection gradient: `tide`, `ember`, `orchid`, `mono` |

## Development

See [AGENTS.md](AGENTS.md).

```bash
make test     # go test -race ./...
make lint     # golangci-lint
make build    # ./bin/dv
```
