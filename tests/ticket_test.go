package tests

import (
	"path/filepath"
	"strings"
	"testing"
)

// A bare ticket id opens its task with --wait in the tickets project, then brings it up again.
func TestBareTicketOpensThenShowsItsTask(t *testing.T) {
	if implementation.kind == "python" {
		t.Skip("Go runner only")
	}
	f := newFixture(t, "hub")
	f.fails(f.agent("HA-125"), "no project here takes tickets")
	f.registry["platform"].(map[string]any)["tickets"] = "garden"
	f.saveRegistry()
	opened := f.ok(f.agent("HA-125"))
	if !strings.Contains(opened.stdout, "garden: task HA-125 opened") {
		t.Fatalf("stdout %q", opened.stdout)
	}
	garden := f.workspace("garden")
	if !strings.Contains(f.read(filepath.Join(garden, "tasks/HA-125/BRIEF.md")), "Brief pending") {
		t.Fatal("ticket task is not waiting for its brief")
	}
	if !f.exists(filepath.Join(garden, ".agent/tasks/HA-125/wait-for-brief")) {
		t.Fatal("no wait marker")
	}
	shown := f.ok(f.agent("HA-125"))
	if !strings.Contains(shown.stdout, "garden: task HA-125 shown") {
		t.Fatalf("second call stdout %q", shown.stdout)
	}
	if f.exists(filepath.Join(f.workspace("admin"), ".agent/tasks/HA-125")) {
		t.Fatal("second call opened a duplicate task")
	}

	// Inside another project a new ticket opens there, an existing one is found where it is.
	f.ok(f.with("PI_PROJECT=admin").agent("HA-126"))
	if !f.exists(filepath.Join(f.workspace("admin"), "tasks/HA-126/BRIEF.md")) {
		t.Fatal("ticket did not open in the current project")
	}
	if r := f.ok(f.with("PI_PROJECT=admin").agent("HA-125")); !strings.Contains(r.stdout, "garden: task HA-125 shown") {
		t.Fatalf("existing ticket from another project %q", r.stdout)
	}

	f.ok(f.agent("task", "close", "garden", "HA-125"))
	metaPath := filepath.Join(garden, ".agent/tasks/HA-125/meta.json")
	meta := readJSON[map[string]any](t, metaPath)
	meta["status"] = "closed"
	f.write(metaPath, mustJSON(t, meta))
	f.fails(f.agent("HA-125"), "closed")
}

// From a client a bare ticket goes to the host of the tickets project.
func TestClientSendsBareTicketToTheTicketsHost(t *testing.T) {
	if implementation.kind == "python" {
		t.Skip("Go runner only")
	}
	f := newFixture(t, "laptop")
	f.registry["platform"].(map[string]any)["tickets"] = "field"
	f.saveRegistry()
	f.ok(f.internal("HA-125"))
	f.ok(f.with("PI_PROJECT=garden").internal("HA-126"))
	calls := f.calls("ssh")
	want := []struct{ host, remote string }{
		{"outpost", "env -u PI_PROJECT fish -lc 'agent --launch HA-125'"},
		{"hub", "env PI_PROJECT=garden fish -lc 'agent --launch HA-126'"},
	}
	if len(calls) != len(want) {
		t.Fatalf("ssh calls %v", calls)
	}
	for i, expected := range want {
		if calls[i].Args[0] != expected.host || calls[i].Args[1] != expected.remote {
			t.Fatalf("call %d: %q, want %q", i, calls[i].Args, []string{expected.host, expected.remote})
		}
	}
}
