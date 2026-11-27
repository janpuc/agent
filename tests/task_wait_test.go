package tests

import (
	"path/filepath"
	"strings"
	"testing"
)

// --wait opens a task without a brief and starts Pi without the first prompt, so the operator can paste
// the brief into the tab.
func TestTaskNewWaitStartsPiWithoutFirstPrompt(t *testing.T) {
	f := newFixture(t, "hub")
	f.fails(f.agent("task", "new", "garden", "--slug", "audit", " "), "--wait")
	f.ok(f.agent("task", "new", "garden", "--wait", "--slug", "audit"))
	garden := f.workspace("garden")
	brief := f.read(filepath.Join(garden, "tasks/audit/BRIEF.md"))
	if !strings.Contains(brief, "Brief pending") {
		t.Fatalf("BRIEF.md %q", brief)
	}
	f.ok(f.internal("--task-run", "garden", "audit"))
	calls := f.calls("pi")
	if len(calls) != 1 {
		t.Fatalf("want one Pi start, got %d", len(calls))
	}
	for _, arg := range calls[0].Args {
		if strings.HasPrefix(arg, "Work on task") {
			t.Fatalf("first prompt sent: %q", calls[0].Args)
		}
	}

	f.ok(f.agent("task", "new", "garden", "TIX-12", "--wait"))
	if !strings.Contains(f.read(filepath.Join(garden, "tasks/TIX-12/BRIEF.md")), "Brief pending") {
		t.Fatal("ticket task without a brief has no pending brief")
	}
	f.ok(f.agent("task", "new", "garden", "--wait", "--slug", "given", "Audit the speakers"))
	if !strings.Contains(f.read(filepath.Join(garden, "tasks/given/BRIEF.md")), "Audit the speakers") {
		t.Fatal("--wait dropped a given brief")
	}
}

// Rex cannot say which app the operator is looking at, so a new task tab is shown in every connected app.
func TestTaskNewShowsTheTabInEveryConnectedApp(t *testing.T) {
	f := newFixture(t, "hub")
	f.open("garden")
	state := loadRexAt(f)
	state.Clients = []any{
		map[string]any{"client_id": "client:laptop", "connection_state": "connected", "info": map[string]any{"kind": "app"}},
		map[string]any{"client_id": "client:desk", "connection_state": "connected", "info": map[string]any{"kind": "app"}},
		map[string]any{"client_id": "client:cli", "connection_state": "connected", "info": map[string]any{"kind": "cli"}},
		map[string]any{"client_id": "client:gone", "connection_state": "disconnected", "info": map[string]any{"kind": "app"}},
	}
	state.saveAt(f)
	result := f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	if strings.Contains(result.stderr, "several Rex apps") {
		t.Fatalf("stderr %q", result.stderr)
	}
	var window string
	for _, session := range loadRexAt(f).Sessions {
		for _, w := range session.Windows {
			if w.Label == "task:audit" {
				window = w.ID
			}
		}
	}
	if window == "" {
		t.Fatal("no task:audit tab")
	}
	shown := map[string]bool{}
	for _, c := range f.calls("rex") {
		if len(c.Args) > 2 && c.Args[0] == "-C" && c.Args[2] == "do" && contains(c.Args, "window_id="+window) {
			shown[c.Args[1]] = true
		}
	}
	if !shown["client:laptop"] || !shown["client:desk"] || shown["client:cli"] || shown["client:gone"] {
		t.Fatalf("task tab shown in %v", shown)
	}
}
