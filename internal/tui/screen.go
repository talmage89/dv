package tui

import (
	"strings"

	"fmt"
	"github.com/charmbracelet/x/ansi"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/talmage89/dv/keymap"
)

// screen is one full-body view.
type screen interface {
	update(a *App, msg tea.Msg) tea.Cmd
	view(a *App, w, h int) string
	hints(a *App) []hint
	help() []helpSection
}

// Optional screen capabilities.
type (
	// initializer screens start work when pushed.
	initializer interface{ init(a *App) tea.Cmd }
	// statusProvider screens fill the status line.
	statusProvider interface{ status(a *App) string }
	// interrupter screens handle ctrl+c themselves.
	interrupter interface{ interrupt(a *App) tea.Cmd }
	// titled screens name themselves in the header breadcrumb.
	titled interface{ title() string }
)

type helpSection struct {
	title string
	keys  []hint
}

// --- modal ---------------------------------------------------------------------

// modal is a dialog drawn over the body: a yes/no confirmation or a help
// sheet dismissed by any key.
type modal struct {
	title   string
	lines   []string
	confirm bool
	danger  bool
	yesText string
	onYes   func() tea.Cmd
	hang    int // continuation indent for wrapped lines (help sheets)
}

func (a *App) confirm(title string, lines []string, danger bool, yesText string, onYes func() tea.Cmd) {
	a.modal = &modal{title: title, lines: lines, confirm: true, danger: danger, yesText: yesText, onYes: onYes}
}

func (a *App) openHelp() {
	var lines []string
	secs := a.top().help()
	keyW := 0
	for _, sec := range secs {
		for _, k := range sec.keys {
			keyW = max(keyW, width(k.key))
		}
	}
	for i, sec := range secs {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, stMuted.Render(strings.ToLower(sec.title)))
		for _, k := range sec.keys {
			lines = append(lines, pad(k.key, keyW)+"   "+stMuted.Render(k.desc))
		}
	}
	a.modal = &modal{title: "keys", lines: lines, hang: keyW + 3}
}

func (m *modal) update(a *App, msg tea.KeyPressMsg) tea.Cmd {
	if !m.confirm {
		a.modal = nil
		return nil
	}
	switch k := msg.String(); {
	case keys.Is(k, keymap.ModalYes):
		a.modal = nil
		if m.onYes != nil {
			return m.onYes()
		}
	case k == "ctrl+c", keys.Is(k, keymap.ModalNo):
		a.modal = nil
	}
	return nil
}

func (m *modal) view(w, h int) string {
	if !m.confirm {
		return m.sheet(w, h)
	}
	boxW := 44
	for _, l := range m.lines {
		boxW = max(boxW, width(l)+6)
	}
	boxW = min(boxW, 90, w-4)
	var body []string
	body = append(body, "")
	avail := boxW - 6
	for _, l := range m.lines {
		if m.hang == 0 || width(l) <= avail || m.hang >= avail {
			body = append(body, wrap(l, avail)...)
			continue
		}
		head := ansi.Cut(l, 0, m.hang)
		for i, part := range wrap(ansi.Cut(l, m.hang, width(l)), avail-m.hang) {
			if i == 0 {
				body = append(body, head+part)
			} else {
				body = append(body, strings.Repeat(" ", m.hang)+part)
			}
		}
	}
	body = append(body, "")
	if m.confirm {
		yes := m.yesText
		if yes == "" {
			yes = "confirm"
		}
		ys := plain
		if m.danger {
			ys = stErr
		}
		body = append(body, keys.Key(keymap.ModalYes)+" "+ys.Render(yes)+"   "+keys.Key(keymap.ModalNo)+" "+stMuted.Render("cancel"))
	} else {
		body = append(body, stMuted.Render("press any key to close"))
	}
	boxH := min(len(body)+2, h)
	indented := make([]string, len(body))
	for i, l := range body {
		indented[i] = " " + l
	}
	b := box(m.title, indented, boxW, boxH, true)
	return center(b, w, h)
}

// sheet renders a help sheet as a plain page, like every other screen.
func (m *modal) sheet(w, h int) string {
	avail := w - 4
	var body []string
	for _, l := range m.lines {
		if width(l) <= avail || m.hang >= avail {
			body = append(body, truncate(l, avail))
			continue
		}
		head := ansi.Cut(l, 0, m.hang)
		for i, part := range wrap(ansi.Cut(l, m.hang, width(l)), avail-m.hang) {
			if i == 0 {
				body = append(body, head+part)
			} else {
				body = append(body, strings.Repeat(" ", m.hang)+part)
			}
		}
	}
	return panel("", body, w, h, true)
}

func (m *modal) hints() []hint {
	if m.confirm {
		return []hint{{keys.Key(keymap.ModalYes), "confirm"}, {keys.Key(keymap.ModalNo), "cancel"}}
	}
	return []hint{{"any key", "close"}}
}

// --- toast & log -----------------------------------------------------------------

type level int

const (
	levelInfo level = iota
	levelWarn
	levelError
)

type toast struct {
	text  string
	level level
	until time.Time
}

func (t *toast) render() string {
	switch t.level {
	case levelError:
		return stErr.Render(t.text)
	case levelWarn:
		return stWarn.Render(t.text)
	default:
		return t.text
	}
}

type logEntry struct {
	at    time.Time
	level level
	text  string
}

const maxLog = 500

func (a *App) logf(lvl level, format string, args ...any) {
	a.log = append(a.log, logEntry{at: time.Now(), level: lvl, text: fmt.Sprintf(format, args...)})
	if len(a.log) > maxLog {
		a.log = a.log[len(a.log)-maxLog:]
	}
}

var (
	_ statusProvider = (*homeScreen)(nil)
	_ statusProvider = (*sweepScreen)(nil)
	_ statusProvider = (*runScreen)(nil)
	_ initializer    = (*sweepScreen)(nil)
	_ initializer    = (*scaleScreen)(nil)
	_ interrupter    = (*runScreen)(nil)
)
