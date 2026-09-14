package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/go-units"
)

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
			created := time.Unix(img.Created, 0)
			rows = append(rows, table.Row{
				mark,
				shortID(img.ID),
				imageName(img),
				strconv.Itoa(useCount),
				yesNo(useCount > 0),
				yesNo(isDanglingImage(img, m.imageChildren)),
				units.HumanSize(float64(img.Size)),
				virtualSizeText(img),
				created.Format("06-01-02 15:04"),
				shortAge(created),
			})
		}
	} else if m.mode == viewVolumes {
		rows = make([]table.Row, 0, len(m.volumes))
		for _, v := range m.volumes {
			rows = append(rows, table.Row{
				v.Name,
				v.Driver,
				v.Scope,
				shortAge(parseCreatedAt(v.CreatedAt)),
				shortPath(v.Mountpoint, 42),
			})
		}
	} else if m.mode == viewPorts {
		visible := m.visiblePorts()
		rows = make([]table.Row, 0, len(visible))
		for _, p := range visible {
			rows = append(rows, table.Row{
				p.host,
				p.docker,
				p.container,
				p.state,
				p.image,
				p.id,
			})
		}
	} else if m.mode == viewCleanup {
		rows = make([]table.Row, 0, len(m.cleanupRows))
		for _, row := range m.cleanupRows {
			rows = append(rows, table.Row{
				row.label,
				strconv.Itoa(row.total),
				units.HumanSize(float64(row.totalSize)),
				strconv.Itoa(row.reclaim),
				units.HumanSize(float64(row.reclaimSize)),
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
		const createdWidth = 18
		nameWidth := max(16, m.width-3-idWidth-4-5-8-10-10-createdWidth-6-tableHorizPadding)

		imageTitle := "Image"
		if m.imageSort == imageSortName {
			imageTitle = ".Image ↑"
		}
		sizeTitle := "Size"
		if m.imageSort == imageSortSize {
			sizeTitle = ".Size ↓"
		}
		// Age and Created share the same underlying key (img.Created), so
		// both header cells reflect the sort marker when it's active.
		ageTitle := "Age"
		createdTitle := "Created"
		if m.imageSort == imageSortAge {
			ageTitle = ".Age ↓"
			createdTitle = ".Created ↓"
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
			{Title: createdTitle, Width: createdWidth},
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
	if m.mode == viewPorts {
		remaining := m.width - 20 - 10 - 7 - 12 - tableHorizPadding
		sharedWidth := max(14, remaining/2)
		m.setColumns([]table.Column{
			{Title: "Host", Width: 20},
			{Title: "Docker", Width: 10},
			{Title: "Container", Width: sharedWidth},
			{Title: "State", Width: 7},
			{Title: "Image", Width: sharedWidth},
			{Title: "ID", Width: 12},
		})
		return
	}
	if m.mode == viewCleanup {
		remaining := m.width - 12 - 14 - 12 - 14 - tableHorizPadding
		labelWidth := max(12, remaining)
		m.setColumns([]table.Column{
			{Title: "Category", Width: labelWidth},
			{Title: "Total", Width: 12},
			{Title: "Size", Width: 14},
			{Title: "Reclaimable", Width: 12},
			{Title: "Reclaim Size", Width: 14},
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

func (m *model) selectedCleanupRow() *cleanupRow {
	if len(m.cleanupRows) == 0 {
		return nil
	}
	return &m.cleanupRows[m.clampCursor(len(m.cleanupRows))]
}

func (m model) rowsLenForMode() int {
	switch m.mode {
	case viewImages:
		return len(m.images)
	case viewVolumes:
		return len(m.volumes)
	case viewPorts:
		return len(m.visiblePorts())
	case viewCleanup:
		return len(m.cleanupRows)
	default:
		return len(m.containers)
	}
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
	case preloadMsg:
		if msg.preloadErr != nil {
			// Preload is best-effort; do not block normal view updates.
			if m.err == nil {
				m.err = fmt.Errorf("preload %s failed: %w", msg.errorSource, msg.preloadErr)
			}
			return m, nil
		}
		m.imagesAll = msg.images
		m.imageUsage = msg.imageUsage
		m.imageChildren = msg.childCount
		m.rebuildImages()
		m.volumes = msg.volumes
		m.ports = msg.ports
		if m.mode != viewContainers {
			m.clampCursor(m.rowsLenForMode())
			m.resizeTable()
			m.syncRows()
		}
		return m, nil
	case diskUsageMsg:
		m.diskUsageLoading = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "docker error"
			m.resizeTable()
			return m, nil
		}
		m.cleanupRows = buildCleanupRows(msg.usage)
		m.err = nil
		if m.mode != viewCleanup {
			return m, nil
		}
		m.clampCursor(len(m.cleanupRows))
		m.resizeTable()
		m.syncRows()
		return m, nil
	case spinner.TickMsg:
		if !m.diskUsageLoading {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case portsMsg:
		m.ports = msg.items
		if m.mode != viewPorts {
			return m, nil
		}
		m.clampCursor(m.rowsLenForMode())
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
			if m.mode == viewCleanup {
				return m, m.startDiskUsage()
			}
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
				case confirmPruneContainers:
					return m, m.pruneContainersCmd()
				case confirmPruneBuildCache:
					return m, m.pruneBuildCacheCmd()
				case confirmPruneAll:
					return m, m.pruneAllCleanupCmd()
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
			} else if m.mode == viewVolumes {
				m.mode = viewPorts
			} else if m.mode == viewPorts {
				m.mode = viewCleanup
			} else {
				m.mode = viewContainers
			}
			m.status = ""
			m.err = nil
			m.clampCursor(m.rowsLenForMode())
			m.resizeTable()
			m.syncRows()
			if m.mode == viewCleanup {
				return m, m.startDiskUsage()
			}
			return m, m.refreshCmd()
		case "I", "shift+tab", "backtab":
			if m.mode == viewContainers {
				m.mode = viewCleanup
			} else if m.mode == viewImages {
				m.mode = viewContainers
			} else if m.mode == viewVolumes {
				m.mode = viewImages
			} else if m.mode == viewPorts {
				m.mode = viewVolumes
			} else {
				m.mode = viewPorts
			}
			m.status = ""
			m.err = nil
			m.clampCursor(m.rowsLenForMode())
			m.resizeTable()
			m.syncRows()
			if m.mode == viewCleanup {
				return m, m.startDiskUsage()
			}
			return m, m.refreshCmd()
		case "g", "ctrl+r":
			m.status = ""
			m.err = nil
			m.resizeTable()
			if m.mode == viewCleanup {
				return m, m.startDiskUsage()
			}
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
			if m.mode != viewContainers {
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
		case "p", "enter":
			if m.mode == viewCleanup {
				row := m.selectedCleanupRow()
				if row == nil {
					return m, nil
				}
				m.confirming = row.action
				m.status = fmt.Sprintf("prune %s (%d reclaimable, %s)? y/n", strings.ToLower(row.label), row.reclaim, units.HumanSize(float64(row.reclaimSize)))
				m.err = nil
				m.resizeTable()
				return m, nil
			}
			if msg.String() == "enter" {
				return m, nil
			}
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
		case "P":
			if m.mode != viewCleanup {
				return m, nil
			}
			m.confirming = confirmPruneAll
			m.status = "prune everything (containers, images, volumes, build cache)? y/n"
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
		case "u":
			if m.mode != viewPorts {
				return m, nil
			}
			m.portsPublished = !m.portsPublished
			m.clampCursor(m.rowsLenForMode())
			m.resizeTable()
			m.syncRows()
			if m.portsPublished {
				m.status = "ports filter: published only"
			} else {
				m.status = "ports filter: all"
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
	if m.mode == viewPorts {
		modeLabel = "ports"
	}
	if m.mode == viewCleanup {
		modeLabel = "cleanup"
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
	} else if m.mode == viewPorts {
		modeShort = "ports"
		if m.portsPublished {
			modeShort += ":published"
		}
	} else if m.mode == viewCleanup {
		modeShort = "cleanup"
	}
	visiblePorts := m.visiblePorts()
	// `len(m.selectedImages)` is the selection count directly — selectedImageIDs
	// would allocate and sort on every render for no reason.
	summary := lipgloss.NewStyle().
		Foreground(lipgloss.Color("109")).
		Render(fmt.Sprintf("containers: %d   images: %d   volumes: %d   ports: %d (active: %d)   selected: %d   mode: %s", len(m.containers), len(m.images), len(m.volumes), len(visiblePorts), countActivePorts(visiblePorts), len(m.selectedImages), modeShort))

	help := renderHelp(m.mode)

	statusStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("111"))
	errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)

	tableView := m.table.View()
	if m.mode == viewCleanup && m.diskUsageLoading {
		tableView = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).
			Render(m.spinner.View() + " computing docker system df... (can take a while)")
	}

	lines := []string{header, summary, "", tableView, "", help}
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
