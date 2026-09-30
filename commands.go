package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/go-units"
)

func (m model) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), m.preloadCmd())
}

func (m model) preloadCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true})
		if err != nil {
			return preloadMsg{preloadErr: err, errorSource: "containers"}
		}
		images, err := m.cli.ImageList(ctx, image.ListOptions{All: true})
		if err != nil {
			return preloadMsg{preloadErr: err, errorSource: "images"}
		}
		volResp, err := m.cli.VolumeList(ctx, volume.ListOptions{})
		if err != nil {
			return preloadMsg{preloadErr: err, errorSource: "volumes"}
		}

		return preloadMsg{
			images:     images,
			imageUsage: buildImageUsageMap(containers),
			childCount: buildImageChildCountMap(images),
			volumes:    volResp.Volumes,
			ports:      buildPortMappings(containers),
		}
	}
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
		case viewPorts:
			containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true})
			if err != nil {
				return errMsg{err: err}
			}
			return portsMsg{items: buildPortMappings(containers)}
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

func (m model) pruneContainersCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		report, err := m.cli.ContainersPrune(ctx, filters.NewArgs())
		if err != nil {
			return actionDoneMsg{err: err, status: "", refresh: false}
		}
		reclaimed := units.HumanSize(float64(report.SpaceReclaimed))
		return actionDoneMsg{err: nil, status: "containers pruned (reclaimed " + reclaimed + ")", refresh: true}
	}
}

func (m model) pruneBuildCacheCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		report, err := m.cli.BuildCachePrune(ctx, build.CachePruneOptions{All: true})
		if err != nil {
			return actionDoneMsg{err: err, status: "", refresh: false}
		}
		reclaimed := units.HumanSize(float64(report.SpaceReclaimed))
		return actionDoneMsg{err: nil, status: "build cache pruned (reclaimed " + reclaimed + ")", refresh: true}
	}
}

// pruneAllCleanupCmd runs every prune category in one command so the UI shows
// a single combined result instead of four separate status flickers.
func (m model) pruneAllCleanupCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		var reclaimed uint64
		var failed []string

		if report, err := m.cli.ContainersPrune(ctx, filters.NewArgs()); err != nil {
			failed = append(failed, "containers: "+err.Error())
		} else {
			reclaimed += report.SpaceReclaimed
		}
		if report, err := m.cli.ImagesPrune(ctx, filters.NewArgs()); err != nil {
			failed = append(failed, "images: "+err.Error())
		} else {
			reclaimed += report.SpaceReclaimed
		}
		if report, err := m.cli.VolumesPrune(ctx, filters.NewArgs()); err != nil {
			failed = append(failed, "volumes: "+err.Error())
		} else {
			reclaimed += report.SpaceReclaimed
		}
		if report, err := m.cli.BuildCachePrune(ctx, build.CachePruneOptions{All: true}); err != nil {
			failed = append(failed, "build cache: "+err.Error())
		} else {
			reclaimed += report.SpaceReclaimed
		}

		status := fmt.Sprintf("pruned all (reclaimed %s)", units.HumanSize(float64(reclaimed)))
		if len(failed) == 0 {
			return actionDoneMsg{status: status, refresh: true}
		}
		return actionDoneMsg{
			err:     fmt.Errorf("some prunes failed: %s", strings.Join(failed, "; ")),
			status:  status,
			refresh: true,
		}
	}
}

// diskUsageCmd fetches the same data behind `docker system df`. Unlike the
// other list calls, this is user-triggered (opened deliberately from the
// Cleanup pane, never on startup) and can genuinely take minutes on a busy
// daemon, hence the much longer timeout.
func (m model) diskUsageCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		usage, err := m.cli.DiskUsage(ctx, types.DiskUsageOptions{})
		return diskUsageMsg{usage: usage, err: err}
	}
}

// startDiskUsage kicks off diskUsageCmd and starts the spinner, marking the
// Cleanup pane as loading so the view can show progress instead of a frozen
// or stale table while the (potentially slow) call is in flight.
func (m *model) startDiskUsage() tea.Cmd {
	m.diskUsageLoading = true
	m.cleanupDetailCategory = confirmNone
	return tea.Batch(m.diskUsageCmd(), m.spinner.Tick)
}
