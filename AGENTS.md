# Working on dv

dv is a general-purpose tool. Keep it free of assumptions about any one
project, company, directory layout, or toolchain beyond git: new behavior is
configurable, with defaults that suit an arbitrary repository.

- Go, standard library first. Match the surrounding code's idiom and comment
  density.
- Before committing, run `gofmt -l .` (no output), `go vet ./...`, and
  `make test`. CI also runs golangci-lint.
- Keys come from `keymap/defaults.conf`. A new key needs an action there and in
  `keymap/keymap.go`; hints, help sheets, and messages read it through `keys`,
  never a literal.
- A user-facing change updates every place it shows: the CLI usage, the home
  hints and help sheet, and the README.
- Commits are lowercase Conventional Commits (`feat:`, `fix:`, `style:`, …).

Leave the repository clean and committed when you finish: no stray build
output, no uncommitted changes, `git status` empty.

## Versions

Every push to `main` that passes CI is released. The version comes from the
commit types since the last tag (`scripts/next-version` previews it), so the
commit type is the version decision. Never tag by hand.

A change is breaking when someone upgrading must change what they do or what
they wrote. Mark it `feat!:` (any type takes `!`) and say what broke in the
body. Breaking means:

- a CLI subcommand or flag removed, renamed, or changed in meaning
- a default key binding changed, or a keymap action removed or renamed
- a config key removed or renamed, or a default changed so existing setups
  behave differently
- `dv run` environment variables, `dv ls` output, exit codes, the shell
  function, or stored state (park marks, config) changed incompatibly

`feat:` adds without breaking: a command, flag, key, action, setting, or
screen. Everything else (`fix:`, `docs:`, `refactor:`, `ci:`, …) is a fix.

| Change | 0.x | 1.0 and later |
|---|---|---|
| breaking | minor | major |
| feat | patch | minor |
| anything else | patch | patch |

Moving to 1.0 is the owner's call, not an agent's.
