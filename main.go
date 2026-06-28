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
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-units"
)

type errMsg struct{ err error }
type containersMsg struct{ items []container.Summary }
type volumesMsg struct{ items []*volume.Volume }
type imagesMsg struct {
	items      []image.Summary
	imageUsage map[string]int
	childCount map[string]int
}
type actionDoneMsg struct {
	err     error
	status  string
	refresh bool
}

type viewMode string

const (
	viewContainers viewMode = "containers"
	viewImages     viewMode = "images"
	viewVolumes    viewMode = "volumes"
)

type confirmAction string

type imageSortMode string

const (
	confirmNone            confirmAction = ""
	confirmDeleteContainer confirmAction = "delete-container"
	confirmDeleteImage     confirmAction = "delete-image"
	confirmDeleteSelected  confirmAction = "delete-selected-images"
	confirmPruneImages     confirmAction = "prune-images"
	confirmDeleteVolume    confirmAction = "delete-volume"
	confirmPruneVolumes    confirmAction = "prune-volumes"
)

const (
	imageSortSize imageSortMode = "size"
	imageSortAge  imageSortMode = "age"
	imageSortName imageSortMode = "name"
)

type model struct {
	cli            *client.Client
	containers     []container.Summary
	volumes        []*volume.Volume
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
	selectedImages map[string]struct{}
	imageUsage     map[string]int
	imageChildren  map[string]int
	imagesAll      []image.Summary
	danglingOnly   bool
	imageSort      imageSortMode

	// Set when user requests interactive shell handoff so main can resume after quitting.
	shellAction string
	shellID     string
}

func newModel(cli *client.Client, showAll bool, cursor int) model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "ID", Width: 12},
			{Title: "Container", Width: 16},
			{Title: "Image", Width: 16},
			{Title: "Ports", Width: 34},
			{Title: "Age", Width: 7},
			{Title: "Exit", Width: 3},
			{Title: "State", Width: 5},
			{Title: "Status", Width: 16},
		}),
		table.WithRows([]table.Row{}),
		table.WithFocused(true),
		table.WithHeight(12),
	)
	t.KeyMap.LineUp.SetKeys("k", "up")
	t.KeyMap.LineDown.SetKeys("j", "down")
	// PageDown default includes space, which is the image-select toggle here.
	t.KeyMap.PageUp.SetKeys("b", "pgup")
	t.KeyMap.PageDown.SetKeys("f", "pgdown")
	// HalfPageDown default includes "d", which is delete here.
	t.KeyMap.HalfPageUp.SetKeys("ctrl+u")
	t.KeyMap.HalfPageDown.SetKeys("ctrl+d")
	// GotoTop default includes "g", which is refresh here; bind to home/end only.
	t.KeyMap.GotoTop.SetKeys("home")
	t.KeyMap.GotoBottom.SetKeys("end", "G")

	styles := table.DefaultStyles()
	styles.Header = styles.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(lipgloss.Color("63")).
		Foreground(lipgloss.Color("252")).
		Bold(true)
	styles.Selected = styles.Selected.
		Foreground(lipgloss.Color("230")).
		Background(lipgloss.Color("24")).
		Bold(true)
	t.SetStyles(styles)

	m := model{
		cli:            cli,
		showAll:        showAll,
		cursor:         cursor,
		table:          t,
		mode:           viewContainers,
		selectedImages: make(map[string]struct{}),
		imageUsage:     make(map[string]int),
		imageChildren:  make(map[string]int),
		imageSort:      imageSortSize,
	}
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
		// `ImageList(All: true)` walks every layer; 5s was too tight on hosts
		// with hundreds of images. Same budget for ContainerList for symmetry.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		switch mode {
		case viewImages:
			items, err := m.cli.ImageList(ctx, image.ListOptions{All: true})
			if err != nil {
				return errMsg{err: err}
			}
			containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true})
			if err != nil {
				return errMsg{err: err}
			}
			return imagesMsg{items: items, imageUsage: buildImageUsageMap(containers), childCount: buildImageChildCountMap(items)}
		case viewVolumes:
			resp, err := m.cli.VolumeList(ctx, volume.ListOptions{})
			if err != nil {
				return errMsg{err: err}
			}
			return volumesMsg{items: resp.Volumes}
		default:
			items, err := m.cli.ContainerList(ctx, container.ListOptions{All: showAll})
			if err != nil {
				return errMsg{err: err}
			}
			return containersMsg{items: items}
		}
	}
}

func (m model) deleteVolumeCmd(name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.cli.VolumeRemove(ctx, name, true)
		return actionDoneMsg{err: err, status: "volume deleted", refresh: true}
	}
}

func (m model) pruneVolumesCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		report, err := m.cli.VolumesPrune(ctx, filters.NewArgs())
		if err != nil {
			return actionDoneMsg{err: err, status: "", refresh: false}
		}
		reclaimed := units.HumanSize(float64(report.SpaceReclaimed))
		return actionDoneMsg{err: nil, status: "volumes pruned (reclaimed " + reclaimed + ")", refresh: true}
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
			// Cell values must be PLAIN TEXT — bubbles/table truncates each
			// cell with runewidth, which counts every byte of an ANSI escape
			// sequence as a column, shifting the row left when styled. Use a
			// distinct ASCII marker for selection instead of color.
			mark := " "
			if _, ok := m.selectedImages[img.ID]; ok {
				mark = "*"
			}
			useCount := imageUseCount(img, m.imageUsage)
			rows = append(rows, table.Row{
				mark,
				shortID(img.ID),
				imageName(img),
				strconv.Itoa(useCount),
				yesNo(useCount > 0),
				yesNo(isDanglingImage(img, m.imageChildren)),
				units.HumanSize(float64(img.Size)),
				virtualSizeText(img),
				shortAge(time.Unix(img.Created, 0)),
			})
		}
	} else if m.mode == viewVolumes {
		rows = make([]table.Row, 0, len(m.volumes))
		for _, v := range m.volumes {
			rows = append(rows, table.Row{
				v.Name,
				v.Driver,
				v.Scope,
				shortAge(parseVolumeCreatedAt(v.CreatedAt)),
				shortPath(v.Mountpoint, 42),
			})
		}
	} else {
		rows = make([]table.Row, 0, len(m.containers))
		for _, c := range m.containers {
			rows = append(rows, table.Row{
				shortID(c.ID),
				containerName(c),
				shortImageRef(c.Image),
				containerPorts(c),
				shortAge(time.Unix(c.Created, 0)),
				containerExitCode(c),
				c.State,
				c.Status,
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

	// Total vertical chrome (header + summary + blank + blank + help) plus a
	// row for the status line and another for the error line when present.
	const baseChrome = 6
	extra := 0
	if m.status != "" {
		extra++
	}
	if m.err != nil {
		extra++
	}
	tableHeight := max(3, m.height-baseChrome-extra)

	// `tableHorizPadding` is the sum of per-cell padding and borders consumed
	// by the bubbles/table layout — extracted so the column-width math reads
	// as "total width minus fixed columns minus chrome".
	const (
		tableHorizPadding = 10
		idWidth           = 12
		minName           = 12
	)

	tableWidth := max(20, m.width-2)
	m.table.SetHeight(tableHeight)
	m.table.SetWidth(tableWidth)

	if m.mode == viewImages {
		nameWidth := max(16, m.width-3-idWidth-4-5-8-10-10-6-tableHorizPadding)

		imageTitle := "Image"
		if m.imageSort == imageSortName {
			imageTitle = ".Image ↑"
		}
		sizeTitle := "Size"
		if m.imageSort == imageSortSize {
			sizeTitle = ".Size ↓"
		}
		ageTitle := "Age"
		if m.imageSort == imageSortAge {
			ageTitle = ".Age ↓"
		}

		m.setColumns([]table.Column{
			{Title: "Sel", Width: 3},
			{Title: "ID", Width: idWidth},
			{Title: imageTitle, Width: nameWidth},
			{Title: "Use", Width: 4},
			{Title: "InUse", Width: 5},
			{Title: "Dangling", Width: 8},
			{Title: sizeTitle, Width: 10},
			{Title: "VSize", Width: 10},
			{Title: ageTitle, Width: 6},
		})
		return
	}
	if m.mode == viewVolumes {
		remaining := m.width - 10 - 8 - 8 - tableHorizPadding
		nameWidth := max(16, remaining/3)
		mountWidth := max(18, remaining-nameWidth)
		m.setColumns([]table.Column{
			{Title: "Name", Width: nameWidth},
			{Title: "Driver", Width: 10},
			{Title: "Scope", Width: 8},
			{Title: "Age", Width: 8},
			{Title: "Mountpoint", Width: mountWidth},
		})
		return
	}

	remaining := m.width - idWidth - 54 - 7 - 6 - 10 - 16 - tableHorizPadding
	nameWidth := max(minName, remaining/3)
	imageWidth := max(12, remaining-nameWidth)

	m.setColumns([]table.Column{
		{Title: "ID", Width: idWidth},
		{Title: "Container", Width: nameWidth},
		{Title: "Image", Width: imageWidth},
		{Title: "Ports", Width: 54},
		{Title: "Age", Width: 7},
		{Title: "Exit", Width: 6},
		{Title: "State", Width: 10},
		{Title: "Status", Width: 16},
	})
}

func (m *model) setColumns(cols []table.Column) {
	rows := m.table.Rows()
	if len(rows) > 0 && len(rows[0]) != len(cols) {
		m.table.SetRows([]table.Row{})
	}
	m.table.SetColumns(cols)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clampCursor pins m.cursor into [0, n) and returns it. Centralized so callers
// can rely on m.cursor staying valid after this call; using a pointer receiver
// is the difference between an in-place clamp and a silent no-op on a copy.
func (m *model) clampCursor(n int) int {
	if n <= 0 {
		m.cursor = 0
		return 0
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= n {
		m.cursor = n - 1
	}
	return m.cursor
}

func (m *model) selected() *container.Summary {
	if len(m.containers) == 0 {
		return nil
	}
	return &m.containers[m.clampCursor(len(m.containers))]
}

func (m *model) selectedVolume() *volume.Volume {
	if len(m.volumes) == 0 {
		return nil
	}
	return m.volumes[m.clampCursor(len(m.volumes))]
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
		m.clampCursor(len(m.containers))
		m.resizeTable()
		m.syncRows()
		m.err = nil
		return m, nil
	case volumesMsg:
		m.volumes = msg.items
		if m.mode != viewVolumes {
			return m, nil
		}
		m.clampCursor(len(m.volumes))
		m.resizeTable()
		m.syncRows()
		m.err = nil
		return m, nil
	case imagesMsg:
		// Remember which image the cursor was on so we can restore it after
		// the sort below — `m.cursor` is positional, so a re-sort silently
		// moves the user's selection to a different image otherwise.
		var prevID string
		if m.mode == viewImages && m.cursor >= 0 && m.cursor < len(m.images) {
			prevID = m.images[m.cursor].ID
		}

		m.imagesAll = msg.items
		m.imageUsage = msg.imageUsage
		m.imageChildren = msg.childCount
		m.rebuildImages()
		present := make(map[string]bool, len(m.images))
		for _, img := range m.images {
			present[img.ID] = true
		}
		for id := range m.selectedImages {
			if !present[id] {
				delete(m.selectedImages, id)
			}
		}
		if m.mode != viewImages {
			return m, nil
		}
		if prevID != "" {
			for i := range m.images {
				if m.images[i].ID == prevID {
					m.cursor = i
					break
				}
			}
		}
		m.clampCursor(len(m.images))
		m.resizeTable()
		m.syncRows()
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
				case confirmDeleteVolume:
					vol := m.selectedVolume()
					if vol == nil {
						m.status = "no volume selected"
						m.resizeTable()
						return m, nil
					}
					return m, m.deleteVolumeCmd(vol.Name)
				case confirmPruneVolumes:
					return m, m.pruneVolumesCmd()
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
			} else if m.mode == viewImages {
				m.mode = viewVolumes
			} else {
				m.mode = viewContainers
			}
			m.status = ""
			m.err = nil
			m.resizeTable()
			m.syncRows()
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
			if m.mode == viewVolumes {
				vol := m.selectedVolume()
				if vol == nil {
					return m, nil
				}
				m.confirming = confirmDeleteVolume
				m.status = fmt.Sprintf("delete volume %s? y/n", vol.Name)
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
			if _, ok := m.selectedImages[img.ID]; ok {
				delete(m.selectedImages, img.ID)
			} else {
				m.selectedImages[img.ID] = struct{}{}
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
				if m.mode == viewVolumes {
					m.confirming = confirmPruneVolumes
					m.status = "prune unused volumes? y/n"
					m.err = nil
					m.resizeTable()
					return m, nil
				}
				return m, nil
			}
			m.confirming = confirmPruneImages
			m.status = "prune dangling images? y/n"
			m.err = nil
			m.resizeTable()
			return m, nil
		case "f":
			if m.mode != viewImages {
				return m, nil
			}
			m.danglingOnly = !m.danglingOnly
			m.rebuildImages()
			m.resizeTable()
			m.syncRows()
			if m.danglingOnly {
				m.status = "filter: dangling only"
			} else {
				m.status = "filter: all images"
			}
			m.err = nil
			return m, nil
		case "s":
			if m.mode != viewImages {
				return m, nil
			}
			switch m.imageSort {
			case imageSortSize:
				m.imageSort = imageSortAge
			case imageSortAge:
				m.imageSort = imageSortName
			default:
				m.imageSort = imageSortSize
			}
			m.rebuildImages()
			m.resizeTable()
			m.syncRows()
			m.status = "sort: " + string(m.imageSort)
			m.err = nil
			return m, nil
		case "x":
			if m.mode == viewContainers {
				sel := m.selected()
				if sel == nil {
					return m, nil
				}
				m.shellAction = "exec"
				m.shellID = sel.ID
				return m, tea.Quit
			}
			if m.mode == viewImages {
				img := m.selectedImage()
				if img == nil {
					return m, nil
				}
				m.shellAction = "run-image-bash"
				m.shellID = imageRunRef(*img)
				return m, tea.Quit
			}
			return m, nil
		case "l":
			if m.mode != viewContainers {
				return m, nil
			}
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
	if m.mode == viewVolumes {
		modeLabel = "volumes"
	}

	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color("231")).
		Background(lipgloss.Color("24")).
		Padding(0, 1).
		Bold(true).
		Render(fmt.Sprintf("wharf [%s]", modeLabel))

	modeShort := "running"
	if m.mode == viewContainers && m.showAll {
		modeShort = "all"
	}
	if m.mode == viewImages {
		modeShort = "images"
		if m.danglingOnly {
			modeShort += ":dangling"
		}
		modeShort += ":sort=" + string(m.imageSort)
	} else if m.mode == viewVolumes {
		modeShort = "volumes"
	}
	// `len(m.selectedImages)` is the selection count directly — selectedImageIDs
	// would allocate and sort on every render for no reason.
	summary := lipgloss.NewStyle().
		Foreground(lipgloss.Color("109")).
		Render(fmt.Sprintf("containers: %d   images: %d   volumes: %d   selected: %d   mode: %s", len(m.containers), len(m.images), len(m.volumes), len(m.selectedImages), modeShort))

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

func runImageBash(ref string) error {
	cmd := exec.Command("docker", "run", "--rm", "-it", ref, "bash")
	return runAttached(cmd)
}

func runAttached(cmd *exec.Cmd) error {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// While attached to child process, capture SIGINT in parent so Ctrl-C can
	// stop the child without terminating wharf itself.
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

	parts := []helpPart{{"j/k", "move"}, {"pg/home/end", "jump"}, {"i/tab", "view"}, {"g", "refresh"}, {"q", "quit"}}
	switch mode {
	case viewContainers:
		parts = []helpPart{
			{"j/k", "move"}, {"pg/home/end", "jump"}, {"i/tab", "view"}, {"a", "toggle"}, {"g", "refresh"},
			{"r", "restart"}, {"d", "delete"}, {"x", "exec"}, {"l", "logs"}, {"q", "quit"},
		}
	case viewImages:
		parts = []helpPart{
			{"j/k", "move"}, {"pg/home/end", "jump"}, {"i/tab", "view"}, {"g", "refresh"}, {"space", "select"},
			{"D", "del sel"}, {"d", "delete"}, {"p", "prune"}, {"f", "dangling"}, {"s", "sort"}, {"x", "run bash"}, {"q", "quit"},
		}
	case viewVolumes:
		parts = []helpPart{
			{"j/k", "move"}, {"pg/home/end", "jump"}, {"i/tab", "view"}, {"g", "refresh"},
			{"d", "delete"}, {"p", "prune"}, {"q", "quit"},
		}
	}

	rendered := make([]string, 0, len(parts))
	for _, p := range parts {
		rendered = append(rendered, lipgloss.JoinHorizontal(lipgloss.Top, keyStyle.Render(p.key), " "+baseStyle.Render(p.msg)))
	}

	return strings.Join(rendered, "  ")
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

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func isDanglingImage(img image.Summary, childCount map[string]int) bool {
	if len(img.RepoTags) == 0 {
		return childCount[normalizeImageID(img.ID)] == 0
	}
	for _, tag := range img.RepoTags {
		if tag != "<none>:<none>" {
			return false
		}
	}
	return childCount[normalizeImageID(img.ID)] == 0
}

func virtualSizeText(img image.Summary) string {
	if img.VirtualSize <= 0 {
		return "-"
	}
	return units.HumanSize(float64(img.VirtualSize))
}

func containerPorts(c container.Summary) string {
	if len(c.Ports) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(c.Ports))
	for _, p := range c.Ports {
		if p.PublicPort > 0 {
			parts = append(parts, strconv.Itoa(int(p.PublicPort))+"->"+strconv.Itoa(int(p.PrivatePort))+"/"+p.Type)
			continue
		}
		parts = append(parts, strconv.Itoa(int(p.PrivatePort))+"/"+p.Type)
	}
	return strings.Join(parts, ",")
}

func containerExitCode(c container.Summary) string {
	if strings.EqualFold(c.State, "running") {
		return "-"
	}
	start := strings.Index(c.Status, "Exited (")
	if start < 0 {
		return "-"
	}
	start += len("Exited (")
	end := strings.Index(c.Status[start:], ")")
	if end < 0 {
		return "-"
	}
	return c.Status[start : start+end]
}

func buildImageUsageMap(containers []container.Summary) map[string]int {
	usage := make(map[string]int)
	for _, c := range containers {
		if c.ImageID != "" {
			usage[c.ImageID]++
		}
		if c.Image != "" {
			usage[c.Image]++
		}
	}
	return usage
}

func buildImageChildCountMap(images []image.Summary) map[string]int {
	children := make(map[string]int)
	for _, img := range images {
		parent := normalizeImageID(img.ParentID)
		if parent == "" {
			continue
		}
		children[parent]++
	}
	return children
}

func normalizeImageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "sha256:")
	return id
}

func imageUseCount(img image.Summary, usage map[string]int) int {
	if n, ok := usage[img.ID]; ok {
		return n
	}
	for _, tag := range img.RepoTags {
		if n, ok := usage[tag]; ok {
			return n
		}
	}
	return 0
}

func (m *model) rebuildImages() {
	filtered := make([]image.Summary, 0, len(m.imagesAll))
	for _, img := range m.imagesAll {
		if m.danglingOnly && !isDanglingImage(img, m.imageChildren) {
			continue
		}
		filtered = append(filtered, img)
	}

	sort.SliceStable(filtered, func(i, j int) bool {
		switch m.imageSort {
		case imageSortAge:
			if filtered[i].Created == filtered[j].Created {
				return filtered[i].Size > filtered[j].Size
			}
			return filtered[i].Created > filtered[j].Created
		case imageSortName:
			ni := imageName(filtered[i])
			nj := imageName(filtered[j])
			if ni == nj {
				return filtered[i].Created > filtered[j].Created
			}
			return ni < nj
		default:
			if filtered[i].Size == filtered[j].Size {
				return filtered[i].Created > filtered[j].Created
			}
			return filtered[i].Size > filtered[j].Size
		}
	})

	m.images = filtered
}

func imageRunRef(img image.Summary) string {
	for _, tag := range img.RepoTags {
		if tag != "<none>:<none>" {
			return tag
		}
	}
	return img.ID
}

func parseVolumeCreatedAt(v string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Now()
	}
	return t
}

func shortPath(s string, maxLen int) string {
	if maxLen <= 3 || len(s) <= maxLen {
		return s
	}
	head := (maxLen - 1) / 2
	tail := maxLen - head - 1
	return s[:head] + "~" + s[len(s)-tail:]
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

func (m *model) selectedImage() *image.Summary {
	if len(m.images) == 0 {
		return nil
	}
	return &m.images[m.clampCursor(len(m.images))]
}

func (m model) selectedImageIDs() []string {
	ids := make([]string, 0, len(m.selectedImages))
	for id := range m.selectedImages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func removeImageWithRetry(cli *client.Client, id string) error {
	// Shorter budgets — a bulk delete of N images was effectively unkillable
	// at 2m+5m per image. If the daemon really needs longer than this, the
	// user can retry from the UI.
	timeouts := []time.Duration{30 * time.Second, 90 * time.Second}
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
	// lastErr is always non-nil here: the loop only exits without returning
	// when both iterations errored, and every iteration assigns lastErr.
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
		case "run-image-bash":
			if err := runImageBash(state.shellID); err != nil {
				fmt.Fprintln(os.Stderr, "run failed:", err)
			}
		case "logs":
			if err := runLogs(state.shellID); err != nil {
				fmt.Fprintln(os.Stderr, "logs failed:", err)
			}
		}
	}
}
