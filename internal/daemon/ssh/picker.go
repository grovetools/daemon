package ssh

import (
	"fmt"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	chssh "github.com/charmbracelet/ssh"
)

// pickerItem represents a selectable entry in the session picker.
type pickerItem struct {
	title       string
	description string
	args        []string // groveterm args to launch on selection
}

func (i pickerItem) Title() string       { return i.title }
func (i pickerItem) Description() string  { return i.description }
func (i pickerItem) FilterValue() string  { return i.title }

// pickerModel is the bubbletea model for the SSH session picker.
type pickerModel struct {
	list      list.Model
	chosen    []string // groveterm args from selected item
	cancelled bool
}

func (m pickerModel) Init() tea.Cmd {
	return nil
}

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Don't intercept filter input
		if m.list.FilterState() == list.Filtering {
			break
		}
		switch msg.String() {
		case "enter":
			if item, ok := m.list.SelectedItem().(pickerItem); ok {
				m.chosen = item.args
			}
			return m, tea.Quit
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height-2)
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m pickerModel) View() string {
	return m.list.View()
}

// runPicker launches the session picker TUI over the SSH session.
// Returns groveterm args for the selected target, or an error if cancelled.
func (s *Server) runPicker(sess chssh.Session, ptyReq chssh.Pty) ([]string, error) {
	items := s.buildPickerItems()
	if len(items) == 0 {
		_, _ = fmt.Fprintln(sess, "No active workspaces or PTY sessions found.")
		return nil, fmt.Errorf("no sessions available")
	}

	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(lipgloss.Color("170"))
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(lipgloss.Color("241"))

	l := list.New(items, delegate, ptyReq.Window.Width, ptyReq.Window.Height-2)
	l.Title = "Grove Sessions"
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(true)

	model := pickerModel{list: l}

	p := tea.NewProgram(model,
		tea.WithInput(sess),
		tea.WithOutput(sess),
		tea.WithAltScreen(),
	)

	finalModel, err := p.Run()
	if err != nil {
		s.ulog.Error("picker program error").Err(err).Log(sess.Context())
		return nil, err
	}

	result := finalModel.(pickerModel)
	if result.cancelled || result.chosen == nil {
		return nil, fmt.Errorf("cancelled")
	}

	// Clear screen before launching groveterm
	_, _ = fmt.Fprint(sess, "\033[H\033[2J")

	return result.chosen, nil
}

// buildPickerItems creates the list items from store workspaces and PTY sessions.
func (s *Server) buildPickerItems() []list.Item {
	var items []list.Item

	// Add workspaces from the store
	if s.store != nil {
		for _, ws := range s.store.GetWorkspaces() {
			branch := ""
			if ws.GitStatus != nil {
				branch = ws.GitStatus.Branch
			}

			desc := ws.Path
			if branch != "" {
				desc = fmt.Sprintf("%s  (%s)", ws.Path, branch)
			}

			items = append(items, pickerItem{
				title:       ws.Name,
				description: desc,
				args:        []string{"--follower", "--workspace", ws.Name},
			})
		}
	}

	// Add daemon PTY sessions
	if s.ptyManager != nil {
		for _, meta := range s.ptyManager.List() {
			desc := fmt.Sprintf("Daemon PTY · %s", meta.CWD)
			items = append(items, pickerItem{
				title:       fmt.Sprintf("pty:%s", meta.Workspace),
				description: desc,
				args:        []string{"--attach", meta.ID},
			})
		}
	}

	return items
}
