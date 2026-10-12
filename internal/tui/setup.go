package tui

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/talmage89/dv/internal/config"
	"github.com/talmage89/dv/keymap"
)

// setupModel asks which repository dv should manage, for a first run where
// none is configured and dv was started outside any git repository.
type setupModel struct {
	ctx   context.Context
	input textinput.Model
	err   string
	saved bool
}

// RunSetup prompts for the repository and saves it to the config. It reports
// whether one was saved.
func RunSetup(ctx context.Context, progOpts ...tea.ProgramOption) (bool, error) {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = "~/code/my-project"
	st := textinput.DefaultStyles(true)
	st.Focused.Text = stBold
	in.SetStyles(st)
	in.Focus()
	m := &setupModel{ctx: ctx, input: in}
	if _, err := tea.NewProgram(m, append([]tea.ProgramOption{tea.WithContext(ctx)}, progOpts...)...).Run(); err != nil {
		return false, err
	}
	return m.saved, nil
}

func (m *setupModel) Init() tea.Cmd { return textinput.Blink }

func (m *setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		text, _, _ := strings.Cut(strings.TrimSpace(msg.Content), "\n")
		msg.Content = text
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	case tea.KeyPressMsg:
		switch k := msg.String(); {
		case k == "ctrl+c", keys.Is(k, keymap.TextCancel):
			return m, tea.Quit
		case keys.Is(k, keymap.TextSubmit):
			if err := config.Set(m.ctx, config.KeyRepo, m.input.Value()); err != nil {
				m.err = err.Error()
				return m, nil
			}
			m.saved = true
			return m, tea.Quit
		}
		m.err = ""
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *setupModel) View() tea.View {
	lines := []string{
		"",
		"  " + stBold.Render("dv"),
		"",
		"  Which git clone should dv manage? Any worktree of it works.",
		"  " + stMuted.Render("Saved as the repository setting; change it later in config."),
		"",
		"  " + stMuted.Render("path ") + m.input.View(),
		"",
	}
	if m.err != "" {
		lines = append(lines, "  "+stErr.Render(m.err), "")
	}
	lines = append(lines, "  "+keys.Key(keymap.TextSubmit)+" "+stMuted.Render("save")+"   "+keys.Key(keymap.TextCancel)+" "+stMuted.Render("quit"))
	return tea.NewView(strings.Join(lines, "\n"))
}
