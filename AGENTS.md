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
