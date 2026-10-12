package tui

import "github.com/talmage89/dv/keymap"

// keys is the resolved keymap. The defaults are compiled in, so a load error
// is a build defect rather than something a user can cause.
var keys = mustLoadKeys()

func mustLoadKeys() *keymap.Keymap {
	k, err := keymap.Load()
	if err != nil {
		panic(err)
	}
	return k
}
