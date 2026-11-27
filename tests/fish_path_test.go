package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The Rex server resolves a tab command with its own PATH, which on macOS lacks Homebrew, so
// project and task tabs must name fish by the absolute path the agent found on its own PATH.
func TestTabsLaunchFishByAbsolutePath(t *testing.T) {
	f := newFixture(t, "hub")
	fish := filepath.Join(f.dir, "bin", "fish")
	must(t, os.WriteFile(fish, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	f.open("garden")
	var pi []string
	for _, c := range f.calls("rex") {
		if len(c.Args) < 4 || c.Args[0] != "api" || c.Args[2] != "session.create" {
			continue
		}
		var body struct {
			InitialWindows []struct {
				Layout struct {
					Block struct {
						Options struct {
							Command []string `json:"command"`
						} `json:"options"`
					} `json:"block"`
				} `json:"layout"`
			} `json:"initial_windows"`
		}
		must(t, json.Unmarshal([]byte(c.Args[3]), &body))
		pi = body.InitialWindows[0].Layout.Block.Options.Command
	}
	if len(pi) == 0 || pi[0] != fish {
		t.Fatalf("pi tab command %q, want %s first", pi, fish)
	}
	f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	for _, c := range f.calls("rex") {
		if len(c.Args) > 4 && c.Args[0] == "window" && c.Args[1] == "new" && c.Args[4] == "task:audit" {
			if !contains(c.Args, fish) || contains(c.Args, "fish") {
				t.Fatalf("task tab %q, want %s", c.Args, fish)
			}
			return
		}
	}
	t.Fatal("task new did not create a task:audit tab")
}
