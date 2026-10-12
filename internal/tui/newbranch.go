package tui

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/talmage89/dv/keymap"
)

// newBranchScreen creates a branch off the base ref in a free worktree.
type newBranchScreen struct {
	name  textinput.Model
	base  textinput.Model
	focus int // 0 = name, 1 = base
	err   string
}

func newNewBranch(a *App, seed string) *newBranchScreen {
	mk := func(placeholder string) textinput.Model {
		in := textinput.New()
		in.Prompt = ""
		in.Placeholder = placeholder
		st := textinput.DefaultStyles(true)
		st.Focused.Placeholder = stMuted
		st.Blurred.Placeholder = stMuted
		st.Focused.Text = stBold
		in.SetStyles(st)
		return in
	}
	s := &newBranchScreen{name: mk("paste or type a branch name"), base: mk(a.repo.Base)}
	s.name.SetValue(seed)
	s.name.CursorEnd()
	s.base.SetValue(a.repo.Base)
	s.name.Focus()
	return s
}

func (s *newBranchScreen) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		text, _, _ := strings.Cut(strings.TrimSpace(msg.Content), "\n")
		msg.Content = strings.TrimSpace(text)
		return s.forward(msg)
	case opDoneMsg:
		if msg.res.err != nil {
			s.err = msg.res.err.Error()
		}
		return nil
	case tea.KeyPressMsg:
		switch k := msg.String(); {
		case keys.Is(k, keymap.TextCancel):
			a.pop()
			return nil
		case keys.Is(k, keymap.NewBranchSwitch):
			s.focus = 1 - s.focus
			if s.focus == 0 {
				s.base.Blur()
				return s.name.Focus()
			}
			s.name.Blur()
			return s.base.Focus()
		case keys.Is(k, keymap.TextSubmit):
			return s.submit(a)
		}
		s.err = ""
		return s.forward(msg)
	}
	return s.forward(msg)
}

func (s *newBranchScreen) forward(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if s.focus == 0 {
		s.name, cmd = s.name.Update(msg)
	} else {
		s.base, cmd = s.base.Update(msg)
	}
	return cmd
}

func (s *newBranchScreen) submit(a *App) tea.Cmd {
	name := strings.TrimSpace(s.name.Value())
	base := strings.TrimSpace(s.base.Value())
	if msg := s.problem(name); msg != "" && a.snap.Branches[name] == nil {
		s.err = msg
		return nil
	}
	repo := a.repo
	if a.snap.Branches[name] != nil {
		// Already exists: open it rather than fail.
		return a.runOp("checking out "+name, func(ctx context.Context) opResult {
			res, err := repo.Checkout(ctx, name)
			if err != nil {
				return opResult{err: friendly(err)}
			}
			return opResult{cd: res.Path, touched: []string{res.Path}, toast: "Opened " + name}
		})
	}
	return a.runOp("creating "+name, func(ctx context.Context) opResult {
		path, err := repo.NewBranch(ctx, name, base)
		if err != nil {
			return opResult{err: friendly(err)}
		}
		return opResult{cd: path, touched: []string{path}, toast: "Created " + name}
	})
}

// problem validates the name as typed.
func (s *newBranchScreen) problem(name string) string {
	switch {
	case name == "":
		return "enter a branch name"
	case !validRefName(name):
		return "not a valid branch name"
	}
	return ""
}

func (s *newBranchScreen) view(a *App, w, h int) string {
	inner := w - 4
	s.name.SetWidth(inner - labelW - 1)
	s.base.SetWidth(inner - labelW - 1)
	body := []string{
		field("name", s.focus == 0, s.name.View()),
		field("base", s.focus == 1, s.base.View()),
		"",
	}
	name := strings.TrimSpace(s.name.Value())
	switch {
	case s.err != "":
		body = append(body, note(s.err, inner, stErr)...)
	case name != "" && a.snap.Branches[name] != nil:
		if b := a.snap.Branches[name]; b.Worktree != "" {
			body = append(body, note("Already exists and is checked out; enter opens it.", inner, stWarn)...)
		} else {
			body = append(body, note("Already exists; enter checks it out.", inner, stWarn)...)
		}
	case name != "" && !validRefName(name):
		body = append(body, note("Not a valid branch name.", inner, stErr)...)
	default:
		free, known := a.freeCount()
		if known && free == 0 {
			body = append(body, note("No free worktree. Go back and press "+keys.Key(keymap.HomeScale)+" to add one first.", inner, stWarn)...)
		} else {
			body = append(body, note("Fetches "+strings.TrimSpace(s.base.Value())+", creates the branch in a free worktree, and opens it.", inner, stMuted)...)
		}
	}
	if a.repo.User != "" && name != "" && !strings.Contains(name, "/") {
		body = append(body, note("Your branches usually start with "+a.repo.User+"/", inner, stMuted)...)
	}
	return panel("", body, w, h, true)
}

func (s *newBranchScreen) hints(a *App) []hint {
	return []hint{{keys.Key(keymap.TextSubmit), "create & open"}, {keys.Key(keymap.NewBranchSwitch), "switch field"}, {keys.Key(keymap.TextCancel), "back"}}
}

func (s *newBranchScreen) help() []helpSection {
	return []helpSection{{"New branch", []hint{
		{keys.Keys(keymap.TextSubmit), "create the branch and open it (opens it if it already exists)"},
		{keys.Keys(keymap.NewBranchSwitch), "switch between name and base"},
		{keys.Keys(keymap.TextCancel), "back"},
	}}}
}

func (s *newBranchScreen) title() string { return "new branch" }
