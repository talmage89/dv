package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// shellInit is the function `eval "$(dv shell-init)"` defines. dv cannot change
// its parent shell's directory, so it writes the target to a file named by
// $DV_CD_FILE and the function changes into it after dv exits. Unlike
// capturing stdout, this leaves dv's own output and the TUI untouched.
const shellInit = `dv() {
  local __dv_cd __dv_rc
  __dv_cd="$(mktemp "${TMPDIR:-/tmp}/dv-cd.XXXXXX")" || { command dv "$@"; return; }
  DV_CD_FILE="$__dv_cd" command dv "$@"
  __dv_rc=$?
  if [ -s "$__dv_cd" ]; then
    builtin cd -- "$(cat "$__dv_cd")" || __dv_rc=$?
  fi
  rm -f -- "$__dv_cd"
  return $__dv_rc
}
`

const rcLine = `eval "$(command dv shell-init)"`

// rcFile picks the startup file for the user's shell.
func rcFile() string {
	home, _ := os.UserHomeDir()
	if strings.HasSuffix(os.Getenv("SHELL"), "zsh") {
		return filepath.Join(home, ".zshrc")
	}
	return filepath.Join(home, ".bashrc")
}

// install reports on and optionally writes the shell integration.
func install(args []string) int {
	write := len(args) > 0 && (args[0] == "--write" || args[0] == "-w")
	rc := rcFile()
	data, _ := os.ReadFile(rc)
	content := string(data)
	hasInit := strings.Contains(content, "dv shell-init")
	hasLegacy := strings.Contains(content, "__resolve_path")

	exe, _ := os.Executable()
	fmt.Printf("binary       %s\n", exe)
	switch {
	case hasInit:
		fmt.Printf("integration  installed in %s\n", rc)
	case write:
		f, err := os.OpenFile(rc, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return fail(err)
		}
		_, err = fmt.Fprintf(f, "\n# dv: lets `dv` change the shell's directory\n%s\n", rcLine)
		f.Close()
		if err != nil {
			return fail(err)
		}
		fmt.Printf("integration  added to %s — open a new shell or run: source %s\n", rc, rc)
	default:
		fmt.Printf("integration  not installed\n\nAdd this line to %s (or run `dv install --write`):\n\n  %s\n", rc, rcLine)
	}
	if hasLegacy {
		fmt.Printf("\n%s still defines the old dv() wrapper (the one calling `dv __resolve_path`).\n"+
			"It keeps working, but remove it once the line above is in place.\n", rc)
	}
	return 0
}
