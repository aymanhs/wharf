package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-units"
)

type errMsg struct{ err error }
type containersMsg struct{ items []container.Summary }
type imagesMsg struct{ items []image.Summary }
type actionDoneMsg struct {
	err     error
	status  string
	refresh bool
}

type viewMode string

const (
	viewContainers viewMode = "containers"
	viewImages     viewMode = "images"
)

type confirmAction string

const (
	confirmNone            confirmAction = ""
	confirmDeleteContainer confirmAction = "delete-container"
	confirmDeleteImage     confirmAction = "delete-image"
	confirmDeleteSelected  confirmAction = "delete-selected-images"
	confirmPruneImages     confirmAction = "prune-images"
)

type model struct {
	cli            *client.Client
	containers     []container.Summary
	images         []image.Summary
	cursor         int
	showAll        bool
	mode           viewMode
	table          table.Model
	width          int
	height         int
	status         string
	err            error
	confirming     confirmAction
	selectedImages map[string]bool

	// Set when user requests interactive shell handoff so main can resume after quitting.
	shellAction string
	shellID     string
}

func newModel(cli *client.Client, showAll bool, cursor int) model {
	cols := []table.Column{
		{Title: "ID", Width: 12},
		{Title: "Container", Width: 20},
		{Title: "Image", Width: 24},
		{Title: "State", Width: 10},
		{Title: "Status", Width: 20},
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows([]table.Row{}),
		table.WithFocused(true),
		table.WithHeight(12),
	)
	t.KeyMap.LineUp.SetKeys("k", "up")
	t.KeyMap.LineDown.SetKeys("j", "down")
	t.KeyMap.PageUp.SetKeys("b")
	t.KeyMap.PageDown.SetKeys("f")
	t.KeyMap.HalfPageUp.SetEnabled(false)
	t.KeyMap.HalfPageDown.SetEnabled(false)
	t.KeyMap.GotoTop.SetEnabled(false)
	t.KeyMap.GotoBottom.SetEnabled(false)

	styles := table.DefaultStyles()
	styles.Header = styles.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(lipgloss.Color("63")).
		Foreground(lipgloss.Color("252")).
		Bold(true)
	styles.Selected = styles.Selected.
		Foreground(lipgloss.Color("230")).
		Background(lipgloss.Color("33")).
		Bold(true)
	t.SetStyles(styles)

	m := model{cli: cli, showAll: showAll, cursor: cursor, table: t, mode: viewContainers, selectedImages: make(map[string]bool)}
	m.table.SetCursor(cursor)
	return m
}

func (m model) Init() tea.Cmd {
	return m.refreshCmd()
}

func (m model) refreshCmd() tea.Cmd {
	showAll := m.showAll
	mode := m.mode
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		switch mode {
		case viewImages:
			items, err := m.cli.ImageList(ctx, image.ListOptions{All: true})
			if err != nil {
				return errMsg{err: err}
			}
			return imagesMsg{items: items}
		default:
			items, err := m.cli.ContainerList(ctx, container.ListOptions{All: showAll})
			if err != nil {
				return errMsg{err: err}
			}
			return containersMsg{items: items}
		}
	}
}

func (m model) restartCmd(id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.cli.ContainerRestart(ctx, id, container.StopOptions{})
		return actionDoneMsg{err: err, status: "container restarted", refresh: true}
	}
}

func (m model) deleteCmd(id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
		return actionDoneMsg{err: err, status: "container deleted", refresh: true}
	}
}

func (m model) deleteImageCmd(id string) tea.Cmd {
	return func() tea.Msg {
		err := removeImageWithRetry(m.cli, id)
		return actionDoneMsg{err: err, status: "image deleted", refresh: true}
	}
}

func (m model) deleteSelectedImagesCmd(ids []string) tea.Cmd {
	return func() tea.Msg {
		ok := 0
		failed := 0
		for _, id := range ids {
			if err := removeImageWithRetry(m.cli, id); err != nil {
				failed++
				continue
			}
			ok++
		}

		if failed == 0 {
			return actionDoneMsg{status: fmt.Sprintf("deleted %d images", ok), refresh: true}
		}
		if ok == 0 {
			return actionDoneMsg{err: fmt.Errorf("failed to delete %d selected images", failed), refresh: true}
		}
		return actionDoneMsg{status: fmt.Sprintf("deleted %d images (%d failed)", ok, failed), refresh: true}
	}
}

func (m model) pruneImagesCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		report, err := m.cli.ImagesPrune(ctx, filters.NewArgs())
		if err != nil {
			return actionDoneMsg{err: err, status: "", refresh: false}
		}
		reclaimed := units.HumanSize(float64(report.SpaceReclaimed))
		return actionDoneMsg{err: nil, status: "images pruned (reclaimed " + reclaimed + ")", refresh: true}
	}
}

func (m *model) syncRows() {
	rows := make([]table.Row, 0)
	if m.mode == viewImages {
		rows = make([]table.Row, 0, len(m.images))
		for _, img := range m.images {
			mark := " "
			if m.selectedImages[img.ID] {
				mark = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true).Render("*")
			}
			rows = append(rows, table.Row{
				mark,
				shortID(img.ID),
				imageName(img),
				units.HumanSize(float64(img.Size)),
				shortAge(time.Unix(img.Created, 0)),
			})
		}
	} else {
		rows = make([]table.Row, 0, len(m.containers))
		for _, c := range m.containers {
			rows = append(rows, table.Row{
				shortID(c.ID),
				containerName(c),
				shortImageRef(c.Image),
				colorState(c.State),
				colorStatus(c.Status),
			})
		}
	}

	m.table.SetRows(rows)
	if len(rows) == 0 {
		m.table.SetCursor(0)
		m.cursor = 0
		return
	}
	if m.cursor >= len(rows) {
		m.cursor = len(rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.table.SetCursor(m.cursor)
}

func (m *model) resizeTable() {
	if m.width <= 0 {
		m.width = 80
	}
	if m.height <= 0 {
		m.height = 24
	}

	headerLines := 3
	footerLines := 3
	extraLines := 0
	if m.status != "" {
		extraLines++
	}
	if m.err != nil {
		extraLines++
	}

	tableHeight := m.height - headerLines - footerLines - extraLines
	if tableHeight < 3 {
		tableHeight = 3
	}

	idWidth := 12
	if m.mode == viewImages {
		markWidth := 3
		sizeWidth := 11
		createdWidth := 11
		nameWidth := m.width - markWidth - idWidth - sizeWidth - createdWidth - 16
		if nameWidth < 20 {
			nameWidth = 20
		}
		m.table.SetHeight(tableHeight)
		m.table.SetWidth(max(20, m.width-2))
		m.table.SetColumns([]table.Column{
			{Title: "Sel", Width: markWidth},
			{Title: "ID", Width: idWidth},
			{Title: "Image", Width: nameWidth},
			{Title: "Size", Width: sizeWidth},
			{Title: "Created", Width: createdWidth},
		})
		return
	}

	statusWidth := m.width / 4
	if statusWidth < 18 {
		statusWidth = 18
	}
	if statusWidth > 30 {
		statusWidth = 30
	}
	stateWidth := 10
	imageWidth := m.width / 5
	if imageWidth < 16 {
		imageWidth = 16
	}
	if imageWidth > 34 {
		imageWidth = 34
	}
	nameWidth := m.width - idWidth - imageWidth - stateWidth - statusWidth - 14
	if nameWidth < 14 {
		nameWidth = 14
	}
	imageWidth = m.width - idWidth - nameWidth - stateWidth - statusWidth - 14
	if imageWidth < 14 {
		imageWidth = 14
	}
	statusWidth = m.width - idWidth - nameWidth - imageWidth - stateWidth - 14
	if statusWidth < 14 {
		statusWidth = 14
	}

	m.table.SetHeight(tableHeight)
	m.table.SetWidth(max(20, m.width-2))
	m.table.SetColumns([]table.Column{
		{Title: "ID", Width: idWidth},
		{Title: "Container", Width: nameWidth},
		{Title: "Image", Width: imageWidth},
		{Title: "State", Width: stateWidth},
		{Title: "Status", Width: statusWidth},
	})
}

func (m model) selected() *container.Summary {
	if len(m.containers) == 0 {
		return nil
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.containers) {
		m.cursor = len(m.containers) - 1
	}
	return &m.containers[m.cursor]
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		m.err = msg.err
		m.status = "docker error"
		return m, nil
	case containersMsg:
		m.containers = msg.items
		if m.mode != viewContainers {
			return m, nil
		}
		if m.cursor >= len(m.containers) {
			m.cursor = max(0, len(m.containers)-1)
		}
		if len(m.containers) == 0 {
			m.cursor = 0
		}
		m.syncRows()
		m.resizeTable()
		m.err = nil
		return m, nil
	case imagesMsg:
		m.images = msg.items
		present := make(map[string]bool, len(m.images))
		for _, img := range m.images {
			present[img.ID] = true
		}
		for id := range m.selectedImages {
			if !present[id] {
				delete(m.selectedImages, id)
			}
		}
		sort.SliceStable(m.images, func(i, j int) bool {
			if m.images[i].Size == m.images[j].Size {
				return m.images[i].Created > m.images[j].Created
			}
			return m.images[i].Size > m.images[j].Size
		})
		if m.mode != viewImages {
			return m, nil
		}
		if m.cursor >= len(m.images) {
			m.cursor = max(0, len(m.images)-1)
		}
		if len(m.images) == 0 {
			m.cursor = 0
		}
		m.syncRows()
		m.resizeTable()
		m.err = nil
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeTable()
		return m, nil
	case actionDoneMsg:
		if msg.err != nil {
			m.status = "action failed"
			m.err = msg.err
			m.resizeTable()
			return m, nil
		}
		m.status = msg.status
		m.err = nil
		m.resizeTable()
		if msg.refresh {
			return m, m.refreshCmd()
		}
		return m, nil
	case tea.KeyMsg:
		if m.confirming != confirmNone {
			switch msg.String() {
			case "y", "Y", "enter":
				confirming := m.confirming
				m.confirming = confirmNone
				switch confirming {
				case confirmDeleteContainer:
					sel := m.selected()
					if sel == nil {
						m.status = "no container selected"
						m.resizeTable()
						return m, nil
					}
					return m, m.deleteCmd(sel.ID)
				case confirmDeleteImage:
					img := m.selectedImage()
					if img == nil {
						m.status = "no image selected"
						m.resizeTable()
						return m, nil
					}
					return m, m.deleteImageCmd(img.ID)
				case confirmDeleteSelected:
					ids := m.selectedImageIDs()
					if len(ids) == 0 {
						m.status = "no images selected"
						m.resizeTable()
						return m, nil
					}
					return m, m.deleteSelectedImagesCmd(ids)
				case confirmPruneImages:
					return m, m.pruneImagesCmd()
				default:
					m.status = ""
					m.resizeTable()
					return m, nil
				}
			case "n", "N", "esc":
				m.confirming = confirmNone
				m.status = "action canceled"
				m.resizeTable()
				return m, nil
			case "q", "ctrl+c":
				return m, tea.Quit
			default:
				return m, nil
			}
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "i", "tab":
			if m.mode == viewContainers {
				m.mode = viewImages
			} else {
				m.mode = viewContainers
			}
			m.status = ""
			m.err = nil
			m.syncRows()
			m.resizeTable()
			return m, m.refreshCmd()
		case "g", "ctrl+r":
			m.status = ""
			m.err = nil
			m.resizeTable()
			return m, m.refreshCmd()
		case "a":
			if m.mode != viewContainers {
				return m, nil
			}
			m.showAll = !m.showAll
			m.status = ""
			m.err = nil
			m.resizeTable()
			return m, m.refreshCmd()
		case "r":
			if m.mode != viewContainers {
				return m, nil
			}
			sel := m.selected()
			if sel == nil {
				return m, nil
			}
			return m, m.restartCmd(sel.ID)
		case "d":
			if m.mode == viewImages {
				img := m.selectedImage()
				if img == nil {
					return m, nil
				}
				m.confirming = confirmDeleteImage
				m.status = fmt.Sprintf("delete image %s? y/n", shortID(img.ID))
				m.err = nil
				m.resizeTable()
				return m, nil
			}
			sel := m.selected()
			if sel == nil {
				return m, nil
			}
			m.confirming = confirmDeleteContainer
			m.status = fmt.Sprintf("delete %s (%s)? y/n", containerName(*sel), shortID(sel.ID))
			m.err = nil
			m.resizeTable()
			return m, nil
		case " ":
			if m.mode != viewImages {
				return m, nil
			}
			img := m.selectedImage()
			if img == nil {
				return m, nil
			}
			if m.selectedImages[img.ID] {
				delete(m.selectedImages, img.ID)
			} else {
				m.selectedImages[img.ID] = true
			}
			m.syncRows()
			return m, nil
		case "D":
			if m.mode != viewImages {
				return m, nil
			}
			n := len(m.selectedImageIDs())
			if n == 0 {
				m.status = "no images selected"
				m.resizeTable()
				return m, nil
			}
			m.confirming = confirmDeleteSelected
			m.status = fmt.Sprintf("delete %d selected images? y/n", n)
			m.err = nil
			m.resizeTable()
			return m, nil
		case "p":
			if m.mode != viewImages {
				return m, nil
			}
			m.confirming = confirmPruneImages
			m.status = "prune dangling images? y/n"
			m.err = nil
			m.resizeTable()
			return m, nil
		case "x":
			if m.mode != viewContainers {
				return m, nil
			}
			sel := m.selected()
			if sel == nil {
				return m, nil
			}
			m.shellAction = "exec"
			m.shellID = sel.ID
			return m, tea.Quit
		case "l":
			sel := m.selected()
			if sel == nil {
				return m, nil
			}
			m.shellAction = "logs"
			m.shellID = sel.ID
			return m, tea.Quit
		}
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	m.cursor = m.table.Cursor()
	return m, cmd
}

func (m model) View() string {
	modeLabel := "containers:running"
	if m.mode == viewContainers && m.showAll {
		modeLabel = "containers:all"
	}
	if m.mode == viewImages {
		modeLabel = "images"
	}

	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color("231")).
		Background(lipgloss.Color("24")).
		Padding(0, 1).
		Bold(true).
		Render(fmt.Sprintf("dock [%s]", modeLabel))

	modeShort := "running"
	if m.mode == viewContainers && m.showAll {
		modeShort = "all"
	}
	if m.mode == viewImages {
		modeShort = "images"
	}
	summary := lipgloss.NewStyle().
		Foreground(lipgloss.Color("109")).
		Render(fmt.Sprintf("containers: %d   images: %d   selected: %d   mode: %s", len(m.containers), len(m.images), len(m.selectedImageIDs()), modeShort))

	help := renderHelp(m.mode)

	statusStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("111"))
	errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)

	lines := []string{header, summary, "", m.table.View(), "", help}
	if m.status != "" {
		if m.confirming != confirmNone {
			lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render(m.status))
		} else {
			lines = append(lines, statusStyle.Render(m.status))
		}
	}
	if m.err != nil {
		lines = append(lines, errStyle.Render("error: "+m.err.Error()))
	}

	return strings.Join(lines, "\n")
}

func containerName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	if len(c.ID) >= 12 {
		return c.ID[:12]
	}
	return c.ID
}

func runExecBash(id string) error {
	cmd := exec.Command("docker", "exec", "-it", id, "bash")
	return runAttached(cmd)
}

func runAttached(cmd *exec.Cmd) error {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// While attached to child process, capture SIGINT in parent so Ctrl-C can
	// stop the child without terminating dock itself.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer func() {
		signal.Stop(sigCh)
		close(sigCh)
	}()

	return cmd.Run()
}

func runLogs(id string) error {
	cmd := exec.Command("docker", "logs", "-f", id)
	return runAttached(cmd)
}

func colorStatus(s string) string {
	ls := strings.ToLower(s)
	switch {
	case strings.Contains(ls, "up"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render(s)
	case strings.Contains(ls, "restart"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render(s)
	case strings.Contains(ls, "exit") || strings.Contains(ls, "dead"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render(s)
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Render(s)
	}
}

func colorState(s string) string {
	ls := strings.ToLower(s)
	switch ls {
	case "running":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true).Render(s)
	case "restarting":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render(s)
	case "exited", "dead":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true).Render(s)
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Render(s)
	}
}

func shortImageRef(ref string) string {
	if idx := strings.Index(ref, "@sha256:"); idx > 0 {
		ref = ref[:idx]
	}
	if idx := strings.LastIndex(ref, ":"); idx > strings.LastIndex(ref, "/") {
		name := ref[:idx]
		tag := ref[idx+1:]
		if len(tag) > 14 {
			tag = tag[:14]
		}
		return name + ":" + tag
	}
	return ref
}

func renderHelp(mode viewMode) string {
	baseStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	keyStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("230")).
		Background(lipgloss.Color("60")).
		Padding(0, 1).
		Bold(true)

	type helpPart struct {
		key string
		msg string
	}

	parts := []helpPart{{"j/k", "move"}, {"i/tab", "view"}, {"g", "refresh"}, {"q", "quit"}}
	if mode == viewContainers {
		parts = []helpPart{
			{"j/k", "move"}, {"i/tab", "view"}, {"a", "toggle"}, {"g", "refresh"},
			{"r", "restart"}, {"d", "delete"}, {"x", "exec"}, {"l", "logs"}, {"q", "quit"},
		}
	} else if mode == viewImages {
		parts = []helpPart{
			{"j/k", "move"}, {"i/tab", "view"}, {"g", "refresh"}, {"space", "select"}, {"D", "del sel"}, {"d", "delete"}, {"p", "prune"}, {"q", "quit"},
		}
	}

	rendered := make([]string, 0, len(parts))
	for _, p := range parts {
		rendered = append(rendered, lipgloss.JoinHorizontal(lipgloss.Top, keyStyle.Render(p.key), " "+baseStyle.Render(p.msg)))
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) >= 12 {
		return id[:12]
	}
	return id
}

func imageName(img image.Summary) string {
	if len(img.RepoTags) == 0 {
		return "<none>:<none>"
	}
	if len(img.RepoTags) == 1 {
		return img.RepoTags[0]
	}
	return img.RepoTags[0] + " +" + strconv.Itoa(len(img.RepoTags)-1)
}

func shortAge(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "now"
	}
	if d < time.Hour {
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	if d < 24*time.Hour {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	if d < 30*24*time.Hour {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	if d < 365*24*time.Hour {
		return strconv.FormatInt(int64(d/(30*24*time.Hour)), 10) + "mo"
	}
	return strconv.FormatInt(int64(d/(365*24*time.Hour)), 10) + "y"
}

func (m model) selectedImage() *image.Summary {
	if len(m.images) == 0 {
		return nil
	}
	idx := m.cursor
	if idx < 0 {
		idx = 0
	}
	if idx >= len(m.images) {
		idx = len(m.images) - 1
	}
	return &m.images[idx]
}

func (m model) selectedImageIDs() []string {
	ids := make([]string, 0, len(m.selectedImages))
	for id, on := range m.selectedImages {
		if on {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func removeImageWithRetry(cli *client.Client, id string) error {
	timeouts := []time.Duration{2 * time.Minute, 5 * time.Minute}
	var lastErr error
	for _, timeout := range timeouts {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		_, err := cli.ImageRemove(ctx, id, image.RemoveOptions{Force: true, PruneChildren: true})
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isDeadlineErr(err) {
			return err
		}
	}
	if lastErr == nil {
		lastErr = context.DeadlineExceeded
	}
	return fmt.Errorf("image delete timed out after retry: %w", lastErr)
}

func isDeadlineErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "context deadline exceeded")
}

func main() {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docker client init failed:", err)
		os.Exit(1)
	}
	defer cli.Close()

	showAll := false
	cursor := 0

	for {
		m := newModel(cli, showAll, cursor)
		finalModel, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
		if err != nil {
			fmt.Fprintln(os.Stderr, "tui error:", err)
			os.Exit(1)
		}

		state, ok := finalModel.(model)
		if !ok {
			os.Exit(1)
		}

		showAll = state.showAll
		cursor = state.cursor

		if state.shellAction == "" {
			break
		}

		switch state.shellAction {
		case "exec":
			if err := runExecBash(state.shellID); err != nil {
				fmt.Fprintln(os.Stderr, "exec failed:", err)
			}
		case "logs":
			if err := runLogs(state.shellID); err != nil {
				fmt.Fprintln(os.Stderr, "logs failed:", err)
			}
		}
	}
}
