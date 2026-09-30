package main

import (
	"testing"

	"github.com/docker/docker/api/types/container"
)

func TestContainerStatusAbbreviatesDurations(t *testing.T) {
	tests := map[string]string{
		"Up 2 minutes":                  "Up 2m",
		"Up Less than a second":         "Up <1s",
		"Exited (1) 3 hours ago":        "Exited (1) 3h ago",
		"Restarting (1) 10 seconds ago": "Restarting (1) 10s ago",
	}

	for status, want := range tests {
		t.Run(status, func(t *testing.T) {
			got := containerStatus(container.Summary{Status: status})
			if got != want {
				t.Fatalf("containerStatus() = %q, want %q", got, want)
			}
		})
	}
}
