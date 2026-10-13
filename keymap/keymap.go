// Package keymap resolves dv's key bindings: defaults.conf, overlaid by an
// optional overrides.conf. Both are compiled in, so changing a binding takes
// a rebuild.
package keymap

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

//go:embed *.conf
var files embed.FS

// Action names something a key can do, as "<scope>.<name>".
type Action string

const (
	HomeOpen     Action = "home.open"
	HomeEscape   Action = "home.escape"
	HomeUp       Action = "home.up"
	HomeDown     Action = "home.down"
	HomePageUp   Action = "home.page-up"
	HomePageDown Action = "home.page-down"
	HomeHelp     Action = "home.help"
	HomeNew      Action = "home.new"
	HomeFresh    Action = "home.fresh"
	HomePark     Action = "home.park"
	HomeDetach   Action = "home.detach"
	HomeDelete   Action = "home.delete"
	HomeCopy     Action = "home.copy"
	HomeSweep    Action = "home.sweep"
	HomeRun      Action = "home.run"
	HomeScale    Action = "home.scale"
	HomeMain     Action = "home.main"
	HomeConfig   Action = "home.config"
	HomeLog      Action = "home.log"
	HomeFetch    Action = "home.fetch"

	ListUp        Action = "list.up"
	ListDown      Action = "list.down"
	ListPageUp    Action = "list.page-up"
	ListPageDown  Action = "list.page-down"
	ListToggle    Action = "list.toggle"
	ListToggleAll Action = "list.toggle-all"
	ListConfirm   Action = "list.confirm"
	ListBack      Action = "list.back"
	ListHelp      Action = "list.help"

	TextSubmit Action = "text.submit"
	TextCancel Action = "text.cancel"

	NewBranchSwitch Action = "newbranch.switch"

	ConfigEdit  Action = "config.edit"
	ConfigPrev  Action = "config.prev"
	ConfigNext  Action = "config.next"
	ConfigReset Action = "config.reset"

	LogSync      Action = "log.sync"
	LogForceSync Action = "log.force-sync"
	LogClear     Action = "log.clear"

	ScaleDown  Action = "scale.down"
	ScaleUp    Action = "scale.up"
	ScaleErase Action = "scale.erase"
	ScaleApply Action = "scale.apply"

	RunWhere       Action = "run.where"
	RunHistoryPrev Action = "run.history-prev"
	RunHistoryNext Action = "run.history-next"
	RunPageUp      Action = "run.page-up"
	RunPageDown    Action = "run.page-down"
	RunTop         Action = "run.top"
	RunBottom      Action = "run.bottom"
	RunZoom        Action = "run.zoom"
	RunFailedOnly  Action = "run.failed-only"
	RunRerunFailed Action = "run.rerun-failed"
	RunRerunAll    Action = "run.rerun-all"
	RunStop        Action = "run.stop"
	RunEdit        Action = "run.edit"

	ModalYes Action = "modal.yes"
	ModalNo  Action = "modal.no"
)

// actions lists every Action; defaults.conf must bind each one.
var actions = []Action{
	HomeOpen, HomeEscape, HomeUp, HomeDown, HomePageUp, HomePageDown, HomeHelp,
	HomeNew, HomeFresh, HomePark, HomeDetach, HomeDelete, HomeCopy,
	HomeSweep, HomeRun, HomeScale, HomeMain, HomeConfig, HomeLog, HomeFetch,
	ListUp, ListDown, ListPageUp, ListPageDown, ListToggle, ListToggleAll,
	ListConfirm, ListBack, ListHelp,
	TextSubmit, TextCancel,
	NewBranchSwitch,
	ConfigEdit, ConfigPrev, ConfigNext, ConfigReset,
	LogSync, LogForceSync, LogClear,
	ScaleDown, ScaleUp, ScaleErase, ScaleApply,
	RunWhere, RunHistoryPrev, RunHistoryNext, RunPageUp, RunPageDown, RunTop,
	RunBottom, RunZoom, RunFailedOnly, RunRerunFailed, RunRerunAll, RunStop, RunEdit,
	ModalYes, ModalNo,
}

// Keymap maps actions to the keys that trigger them.
type Keymap struct {
	bindings map[Action][]string
	// Warnings describes override lines that were ignored and keys bound to
	// more than one action in a scope.
	Warnings []string
}

// Load reads the compiled-in defaults and overrides. A defaults file that
// misses or misnames an action is an error; a bad override is a warning.
func Load() (*Keymap, error) {
	defaults, err := files.ReadFile("defaults.conf")
	if err != nil {
		return nil, err
	}
	k := &Keymap{bindings: map[Action][]string{}}
	if errs := k.apply(defaults); len(errs) > 0 {
		return nil, fmt.Errorf("defaults.conf: %s", strings.Join(errs, "; "))
	}
	for _, action := range actions {
		if _, ok := k.bindings[action]; !ok {
			return nil, fmt.Errorf("defaults.conf: %s is not bound", action)
		}
	}
	overrides, err := files.ReadFile("overrides.conf")
	switch {
	case err == nil:
		for _, msg := range k.apply(overrides) {
			k.Warnings = append(k.Warnings, "overrides.conf: "+msg)
		}
	case !errors.Is(err, fs.ErrNotExist):
		k.Warnings = append(k.Warnings, "overrides.conf: "+err.Error())
	}
	k.Warnings = append(k.Warnings, k.conflicts()...)
	return k, nil
}

// apply reads binding lines into k and returns a message per bad line.
func (k *Keymap) apply(data []byte) []string {
	var errs []string
	for n, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "#"); i >= 0 && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		action := Action(fields[0])
		if !slices.Contains(actions, action) {
			errs = append(errs, fmt.Sprintf("line %d: unknown action %q", n+1, action))
			continue
		}
		k.bindings[action] = fields[1:]
	}
	return errs
}

// conflicts reports keys bound to two actions of the same scope.
func (k *Keymap) conflicts() []string {
	var out []string
	owner := map[string]Action{}
	for _, action := range actions {
		scope, _, _ := strings.Cut(string(action), ".")
		for _, key := range k.bindings[action] {
			id := scope + "\x00" + key
			if other, ok := owner[id]; ok {
				out = append(out, fmt.Sprintf("%s is bound to both %s and %s; %s wins where both apply", key, other, action, other))
				continue
			}
			owner[id] = action
		}
	}
	return out
}

// Is reports whether key, as bubbletea names a key press, triggers action.
func (k *Keymap) Is(key string, action Action) bool {
	if key == " " {
		key = "space"
	}
	return slices.Contains(k.bindings[action], key)
}

// Key is the display name of action's first key, for hint lines.
func (k *Keymap) Key(action Action) string {
	if keys := k.bindings[action]; len(keys) > 0 {
		return display(keys[0])
	}
	return "—"
}

// Keys is the display names of every key bound to action, for help sheets.
func (k *Keymap) Keys(action Action) string {
	keys := k.bindings[action]
	if len(keys) == 0 {
		return "—"
	}
	names := make([]string, len(keys))
	for i, key := range keys {
		names[i] = display(key)
	}
	return strings.Join(names, " ")
}

// Pair joins the first keys of two actions, as in "↑/↓".
func (k *Keymap) Pair(first, second Action) string {
	return k.Key(first) + "/" + k.Key(second)
}

var symbols = map[string]string{
	"up":     "↑",
	"down":   "↓",
	"left":   "←",
	"right":  "→",
	"pgdown": "pgdn",
}

func display(key string) string {
	if s, ok := symbols[key]; ok {
		return s
	}
	return key
}
