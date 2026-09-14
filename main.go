package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/docker/docker/client"
)

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
