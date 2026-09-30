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
	"github.com/charmbracelet/lipgloss"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-units"
)

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

func renderHelp(mode viewMode, cleanupDetail bool) string {
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

	parts := []helpPart{{"j/k", "move"}, {"pg/home/end", "jump"}, {"i/tab", "next"}, {"I/S-tab", "prev"}, {"g", "refresh"}, {"q", "quit"}}
	switch mode {
	case viewContainers:
		parts = []helpPart{
			{"j/k", "move"}, {"pg/home/end", "jump"}, {"i/tab", "next"}, {"I/S-tab", "prev"}, {"a", "toggle"}, {"g", "refresh"},
			{"r", "restart"}, {"d", "delete"}, {"x", "exec"}, {"l", "logs"}, {"q", "quit"},
		}
	case viewImages:
		parts = []helpPart{
			{"j/k", "move"}, {"pg/home/end", "jump"}, {"i/tab", "next"}, {"I/S-tab", "prev"}, {"g", "refresh"}, {"space", "select"},
			{"D", "del sel"}, {"d", "delete"}, {"p", "prune"}, {"f", "dangling"}, {"s", "sort"}, {"x", "run bash"}, {"q", "quit"},
		}
	case viewVolumes:
		parts = []helpPart{
			{"j/k", "move"}, {"pg/home/end", "jump"}, {"i/tab", "next"}, {"I/S-tab", "prev"}, {"g", "refresh"},
			{"d", "delete"}, {"p", "prune"}, {"q", "quit"},
		}
	case viewPorts:
		parts = []helpPart{
			{"j/k", "move"}, {"pg/home/end", "jump"}, {"i/tab", "next"}, {"I/S-tab", "prev"}, {"g", "refresh"},
			{"u", "published"},
			{"q", "quit"},
		}
	case viewCleanup:
		if cleanupDetail {
			parts = []helpPart{
				{"j/k", "move"}, {"esc/enter", "back"}, {"p", "prune"}, {"q", "quit"},
			}
		} else {
			parts = []helpPart{
				{"j/k", "move"}, {"i/tab", "next"}, {"I/S-tab", "prev"}, {"g", "refresh"},
				{"enter", "view items"}, {"p", "prune row"}, {"P", "prune all"}, {"q", "quit"},
			}
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
	seen := make(map[string]struct{}, len(c.Ports))
	for _, p := range c.Ports {
		if p.PublicPort == 0 {
			continue
		}
		entry := strconv.Itoa(int(p.PublicPort))
		if _, ok := seen[entry]; ok {
			continue
		}
		seen[entry] = struct{}{}
		parts = append(parts, entry)
	}
	if len(parts) == 0 {
		return "-"
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

func buildPortMappings(containers []container.Summary) []portMapping {
	rows := make([]portMapping, 0)
	for _, c := range containers {
		state := "down"
		active := false
		if strings.EqualFold(c.State, "running") {
			state = "active"
			active = true
		}
		if len(c.Ports) == 0 {
			rows = append(rows, portMapping{
				host:      "-",
				docker:    "-",
				container: containerName(c),
				state:     state,
				image:     shortImageRef(c.Image),
				id:        shortID(c.ID),
				active:    active,
			})
			continue
		}
		for _, p := range c.Ports {
			host := "-"
			if p.PublicPort > 0 {
				ip := p.IP
				if ip == "" {
					ip = "0.0.0.0"
				}
				host = ip + ":" + strconv.Itoa(int(p.PublicPort))
			}
			rows = append(rows, portMapping{
				host:      host,
				docker:    strconv.Itoa(int(p.PrivatePort)) + "/" + p.Type,
				container: containerName(c),
				state:     state,
				image:     shortImageRef(c.Image),
				id:        shortID(c.ID),
				active:    active,
			})
		}
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].host == rows[j].host {
			if rows[i].docker == rows[j].docker {
				if rows[i].active == rows[j].active {
					return rows[i].container < rows[j].container
				}
				return rows[i].active && !rows[j].active
			}
			return rows[i].docker < rows[j].docker
		}
		if rows[i].host == "-" {
			return false
		}
		if rows[j].host == "-" {
			return true
		}
		return rows[i].host < rows[j].host
	})

	return rows
}

func countActivePorts(items []portMapping) int {
	n := 0
	for _, p := range items {
		if p.active {
			n++
		}
	}
	return n
}

func (m model) visiblePorts() []portMapping {
	if !m.portsPublished {
		return m.ports
	}
	visible := make([]portMapping, 0, len(m.ports))
	for _, p := range m.ports {
		if p.host == "-" {
			continue
		}
		visible = append(visible, p)
	}
	return visible
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
	// Expand each image into one row per RepoTag so that an image tagged
	// under multiple names is visible under each. Untagged images (no
	// RepoTags) still surface as a single row.
	filtered := make([]image.Summary, 0, len(m.imagesAll))
	for _, img := range m.imagesAll {
		if m.danglingOnly && !isDanglingImage(img, m.imageChildren) {
			continue
		}
		if len(img.RepoTags) <= 1 {
			filtered = append(filtered, img)
			continue
		}
		for _, tag := range img.RepoTags {
			row := img
			row.RepoTags = []string{tag}
			filtered = append(filtered, row)
		}
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

func parseCreatedAt(v string) time.Time {
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

// buildCleanupRows summarizes a `docker system df`-equivalent DiskUsage
// response into the four rows shown on the Cleanup pane, matching the
// "reclaimable" definitions the docker CLI itself uses: an image counts as
// reclaimable when no container references it, a container when it isn't
// running, a volume when nothing mounts it, and a build cache record when it
// isn't currently in use.
func buildCleanupRows(usage types.DiskUsage) []cleanupRow {
	images := cleanupRow{label: "Images", action: confirmPruneImages}
	for _, img := range usage.Images {
		images.total++
		images.totalSize += img.Size
		if img.Containers == 0 {
			images.reclaim++
			images.reclaimSize += img.Size
		}
	}

	containers := cleanupRow{label: "Containers", action: confirmPruneContainers}
	for _, c := range usage.Containers {
		containers.total++
		containers.totalSize += c.SizeRw
		if !strings.EqualFold(c.State, "running") {
			containers.reclaim++
			containers.reclaimSize += c.SizeRw
		}
	}

	volumes := cleanupRow{label: "Volumes", action: confirmPruneVolumes}
	for _, v := range usage.Volumes {
		volumes.total++
		var size int64
		if v.UsageData != nil {
			size = v.UsageData.Size
			if size < 0 {
				size = 0
			}
		}
		volumes.totalSize += size
		if v.UsageData != nil && v.UsageData.RefCount == 0 {
			volumes.reclaim++
			volumes.reclaimSize += size
		}
	}

	buildCache := cleanupRow{label: "Build Cache", action: confirmPruneBuildCache}
	for _, rec := range usage.BuildCache {
		buildCache.total++
		buildCache.totalSize += rec.Size
		if !rec.InUse {
			buildCache.reclaim++
			buildCache.reclaimSize += rec.Size
		}
	}

	return []cleanupRow{images, containers, volumes, buildCache}
}

// buildCleanupDetailRows lists the individual items behind one Cleanup pane
// row's reclaimable count/size — what pruning that category would actually
// remove. Columns are deliberately generic (ID/Name, Info, Size, Age) since
// each category's underlying object looks different.
func buildCleanupDetailRows(usage types.DiskUsage, category confirmAction) []table.Row {
	rows := make([]table.Row, 0)
	switch category {
	case confirmPruneImages:
		for _, img := range usage.Images {
			if img.Containers != 0 {
				continue
			}
			rows = append(rows, table.Row{
				shortID(img.ID),
				imageName(*img),
				units.HumanSize(float64(img.Size)),
				shortAge(time.Unix(img.Created, 0)),
			})
		}
	case confirmPruneContainers:
		for _, c := range usage.Containers {
			if strings.EqualFold(c.State, "running") {
				continue
			}
			rows = append(rows, table.Row{
				shortID(c.ID),
				containerName(*c) + " (" + c.State + ")",
				units.HumanSize(float64(c.SizeRw)),
				shortAge(time.Unix(c.Created, 0)),
			})
		}
	case confirmPruneVolumes:
		for _, v := range usage.Volumes {
			if v.UsageData == nil || v.UsageData.RefCount != 0 {
				continue
			}
			size := v.UsageData.Size
			if size < 0 {
				size = 0
			}
			rows = append(rows, table.Row{
				v.Name,
				v.Driver,
				units.HumanSize(float64(size)),
				shortAge(parseCreatedAt(v.CreatedAt)),
			})
		}
	case confirmPruneBuildCache:
		for _, rec := range usage.BuildCache {
			if rec.InUse {
				continue
			}
			desc := rec.Description
			if len(desc) > 40 {
				desc = desc[:40]
			}
			rows = append(rows, table.Row{
				shortID(rec.ID),
				desc,
				units.HumanSize(float64(rec.Size)),
				shortAge(rec.CreatedAt),
			})
		}
	}
	return rows
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
