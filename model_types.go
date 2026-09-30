package main

import (
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

type errMsg struct{ err error }
type containersMsg struct{ items []container.Summary }
type portsMsg struct{ items []portMapping }
type volumesMsg struct{ items []*volume.Volume }
type imagesMsg struct {
	items      []image.Summary
	imageUsage map[string]int
	childCount map[string]int
}
type preloadMsg struct {
	images      []image.Summary
	imageUsage  map[string]int
	childCount  map[string]int
	volumes     []*volume.Volume
	ports       []portMapping
	preloadErr  error
	errorSource string
}

type diskUsageMsg struct {
	usage types.DiskUsage
	err   error
}

type portMapping struct {
	host      string
	docker    string
	container string
	state     string
	image     string
	id        string
	active    bool
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
	viewPorts      viewMode = "ports"
	viewCleanup    viewMode = "cleanup"
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
	confirmPruneContainers confirmAction = "prune-containers"
	confirmPruneBuildCache confirmAction = "prune-build-cache"
	confirmPruneAll        confirmAction = "prune-all"
)

// cleanupRow is one category summarized on the Cleanup pane, mirroring a row
// of `docker system df`.
type cleanupRow struct {
	label       string
	total       int
	totalSize   int64
	reclaim     int
	reclaimSize int64
	action      confirmAction
}

const (
	imageSortSize imageSortMode = "size"
	imageSortAge  imageSortMode = "age"
	imageSortName imageSortMode = "name"
)

type model struct {
	cli                   *client.Client
	containers            []container.Summary
	ports                 []portMapping
	volumes               []*volume.Volume
	images                []image.Summary
	cursor                int
	showAll               bool
	mode                  viewMode
	table                 table.Model
	width                 int
	height                int
	status                string
	err                   error
	confirming            confirmAction
	selectedImages        map[string]struct{}
	imageUsage            map[string]int
	imageChildren         map[string]int
	imagesAll             []image.Summary
	danglingOnly          bool
	imageSort             imageSortMode
	portsPublished        bool
	cleanupRows           []cleanupRow
	diskUsage             types.DiskUsage
	cleanupDetailCategory confirmAction
	diskUsageLoading      bool
	spinner               spinner.Model

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
			{Title: "Ports", Width: 16},
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

	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))

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
		spinner:        sp,
	}
	m.table.SetCursor(cursor)
	return m
}
