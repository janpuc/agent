package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A coordinator hands a task a message through its state folder, never its terminal, and a closed
// task gets none.
func TestTaskSendQueuesAMessageForTheSession(t *testing.T) {
	if implementation.kind == "python" {
		t.Skip("Go runner only")
	}
	f := newFixture(t, "hub")
	f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	f.ok(f.with("PI_PROJECT=garden").agent("task", "send", "audit", "Check", "the bridge too"))
	inbox := filepath.Join(f.workspace("garden"), ".agent/tasks/audit/messages")
	entries, err := os.ReadDir(inbox)
	must(t, err)
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".md") {
		t.Fatalf("messages %v", entries)
	}
	if text := f.read(filepath.Join(inbox, entries[0].Name())); text != "Check the bridge too\n" {
		t.Fatalf("message %q", text)
	}
	for _, c := range f.calls("rex") {
		if len(c.Args) > 0 && c.Args[0] == "send" {
			t.Fatalf("typed into a tab: %q", c.Args)
		}
	}
	f.fails(f.agent("task", "send", "garden", "audit", " "), "empty")
	metaPath := filepath.Join(f.workspace("garden"), ".agent/tasks/audit/meta.json")
	meta := readJSON[map[string]any](t, metaPath)
	meta["status"] = "closed"
	f.write(metaPath, mustJSON(t, meta))
	f.fails(f.agent("task", "send", "garden", "audit", "Late"), "closed")
}
