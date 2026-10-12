package tui

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/talmage89/dv/internal/config"
	"github.com/talmage89/dv/internal/pool"
	"github.com/talmage89/dv/keymap"
)

// configScreen edits dv's settings, stored in `git config --global dev.*`.
type configScreen struct {
	cursor  int
	editing bool
	input   textinput.Model
	err     string
}

func newConfigScreen() *configScreen {
	in := textinput.New()
	in.Prompt = ""
	st := textinput.DefaultStyles(true)
	st.Focused.Text = stBold
	in.SetStyles(st)
	return &configScreen{input: in}
}

func (s *configScreen) spec() config.Spec { return config.Specs[s.cursor] }

func (s *configScreen) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		if s.editing {
			text, _, _ := strings.Cut(strings.TrimSpace(msg.Content), "\n")
			msg.Content = text
			var cmd tea.Cmd
			s.input, cmd = s.input.Update(msg)
			return cmd
		}
	case tea.KeyPressMsg:
		if s.editing {
			switch k := msg.String(); {
			case keys.Is(k, keymap.TextCancel):
				s.editing = false
				s.err = ""
				return nil
			case keys.Is(k, keymap.TextSubmit):
				return s.save(a, s.input.Value())
			}
			var cmd tea.Cmd
			s.input, cmd = s.input.Update(msg)
			return cmd
		}
		spec := s.spec()
		v := a.cfg.Get(spec.Key)
		k := msg.String()
		switch {
		case keys.Is(k, keymap.ListHelp):
			a.openHelp()
		case keys.Is(k, keymap.ListBack):
			a.pop()
		case keys.Is(k, keymap.ListUp):
			s.cursor = max(s.cursor-1, 0)
			s.err = ""
		case keys.Is(k, keymap.ListDown):
			s.cursor = min(s.cursor+1, len(config.Specs)-1)
			s.err = ""
		case keys.Is(k, keymap.ConfigEdit), keys.Is(k, keymap.ListToggle):
			if v.Source == config.SourceEnv {
				s.err = "Set by $" + v.EnvVar + " in your environment; change it there."
				return nil
			}
			if spec.Bool {
				return s.save(a, strconv.FormatBool(!a.cfg.Bool(spec.Key)))
			}
			if len(spec.Options) > 0 {
				return s.save(a, cycle(spec.Options, v.Value, 1))
			}
			if !keys.Is(k, keymap.ConfigEdit) {
				return nil
			}
			s.editing = true
			s.err = ""
			s.input.SetValue(v.Value)
			s.input.CursorEnd()
			return s.input.Focus()
		case keys.Is(k, keymap.ConfigPrev), keys.Is(k, keymap.ConfigNext):
			if len(spec.Options) == 0 || v.Source == config.SourceEnv {
				return nil
			}
			step := 1
			if keys.Is(k, keymap.ConfigPrev) {
				step = -1
			}
			return s.save(a, cycle(spec.Options, v.Value, step))
		case keys.Is(k, keymap.ConfigReset):
			if v.Source != config.SourceGit {
				return nil
			}
			if err := config.Unset(a.ctx, spec.Key); err != nil {
				s.err = err.Error()
				return nil
			}
			return s.reload(a, "Reset "+spec.Label+" to its default")
		}
	}
	if s.editing {
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return cmd
	}
	return nil
}

func (s *configScreen) save(a *App, value string) tea.Cmd {
	spec := s.spec()
	if err := config.Set(a.ctx, spec.Key, value); err != nil {
		s.err = err.Error()
		return nil
	}
	s.editing = false
	return s.reload(a, "Saved "+spec.Label)
}

// reload re-resolves config and the repo so changes take effect immediately.
func (s *configScreen) reload(a *App, toast string) tea.Cmd {
	cfg, err := config.Load(context.Background())
	if err != nil {
		s.err = err.Error()
		return nil
	}
	repo, err := pool.Open(a.ctx, cfg)
	if err != nil {
		s.err = "Saved, but the repo no longer resolves: " + err.Error()
		a.cfg = cfg
		return nil
	}
	snap, err := repo.Snapshot(a.ctx)
	if err != nil {
		s.err = err.Error()
		return nil
	}
	a.cfg, a.repo, a.snap = cfg, repo, snap
	a.notify(levelInfo, toast)
	return tea.Batch(a.loadLive(), a.refreshStatuses(nil), a.startAnimation())
}

func (s *configScreen) view(a *App, w, h int) string {
	inner := w - 4
	const labelW = 20
	var body []string
	for i, spec := range config.Specs {
		v := a.cfg.Get(spec.Key)
		selected := i == s.cursor
		bar := "  "
		label := pad(strings.ToLower(spec.Label), labelW)
		if selected {
			bar = selMarker() + " "
			label = rainbow(label)
		}
		if selected && s.editing {
			body = append(body, bar+label+s.input.View())
			continue
		}
		src := stMuted.Render(v.SourceLabel())
		if v.Source == config.SourceEnv {
			src = stWarn.Render(v.SourceLabel())
		}
		body = append(body, bar+label+spread(s.value(a, spec, v), src, inner-labelW-2))
	}
	body = append(body, "")
	indent := strings.Repeat(" ", labelW+2)
	spec := s.spec()
	help := spec.Help
	if spec.EnvVar != "" {
		help += " $" + spec.EnvVar + " overrides it."
	}
	for _, l := range wrap(help, inner-labelW-2) {
		body = append(body, indent+stMuted.Render(l))
	}
	if s.err != "" {
		body = append(body, "")
		for _, l := range wrap(s.err, inner-labelW-2) {
			body = append(body, indent+stErr.Render(l))
		}
	}
	body = append(body, "")
	integration := "shell integration installed"
	if !a.opts.CanCD {
		integration = stWarn.Render("shell integration not installed") + stMuted.Render(", run dv install")
	}
	body = append(body, indent+stMuted.Render("settings live in your global git config under [dev]"))
	body = append(body, indent+stMuted.Render(integration+" · dv "+a.opts.Version))
	return panel("", body, w, h, true)
}

// value renders a setting's current value for its row.
func (s *configScreen) value(a *App, spec config.Spec, v config.Value) string {
	switch {
	case spec.Bool:
		if a.cfg.Bool(spec.Key) {
			return "on"
		}
		return stMuted.Render("off")
	case len(spec.Options) > 0:
		var parts []string
		for _, o := range spec.Options {
			if o == v.Value {
				parts = append(parts, o)
			} else {
				parts = append(parts, stMuted.Render(o))
			}
		}
		return strings.Join(parts, "   ")
	case v.Value == "":
		return stMuted.Render("none")
	case spec.Path:
		return tildify(v.Value, a.home)
	}
	return v.Value
}

func (s *configScreen) hints(a *App) []hint {
	if s.editing {
		return []hint{{keys.Key(keymap.TextSubmit), "save"}, {keys.Key(keymap.TextCancel), "cancel"}}
	}
	move := hint{keys.Pair(keymap.ListUp, keymap.ListDown), "move"}
	reset := hint{keys.Key(keymap.ConfigReset), "reset"}
	back := hint{keys.Key(keymap.ListBack), "back"}
	if s.spec().Bool {
		return []hint{move, {keys.Key(keymap.ListToggle), "toggle"}, reset, back}
	}
	if len(s.spec().Options) > 0 {
		return []hint{move, {keys.Pair(keymap.ConfigPrev, keymap.ConfigNext), "choose"}, reset, back}
	}
	return []hint{move, {keys.Key(keymap.ConfigEdit), "edit"}, reset, back}
}

func (s *configScreen) help() []helpSection {
	return []helpSection{{"Config", []hint{
		{keys.Keys(keymap.ListUp), "move up"},
		{keys.Keys(keymap.ListDown), "move down"},
		{keys.Keys(keymap.ConfigEdit), "edit (toggles on/off settings)"},
		{keys.Keys(keymap.ListToggle), "toggle an on/off setting"},
		{keys.Keys(keymap.ConfigPrev) + " / " + keys.Keys(keymap.ConfigNext), "choose an option"},
		{keys.Keys(keymap.ConfigReset), "reset to default (removes it from git config)"},
		{keys.Keys(keymap.TextSubmit) + " / " + keys.Keys(keymap.TextCancel), "save / cancel an edit"},
		{keys.Keys(keymap.ListBack), "back"},
	}}}
}

func (s *configScreen) title() string { return "config" }

// cycle returns the option step places after current, wrapping around.
func cycle(options []string, current string, step int) string {
	i := slices.Index(options, current)
	return options[((i+step)%len(options)+len(options))%len(options)]
}
