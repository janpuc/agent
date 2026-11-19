package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskLifecycleNeverDeletesData(t *testing.T) {
	f := newFixture(t, "hub")
	f.ok(f.agent("task", "new", "garden", "--slug", "safe-close", "Keep all artifacts"))
	garden := f.workspace("garden")
	task := filepath.Join(garden, "tasks/safe-close")
	meta := filepath.Join(garden, ".agent/tasks/safe-close/meta.json")
	brief := f.read(filepath.Join(task, "BRIEF.md"))
	if !strings.HasPrefix(brief, "---\nid: safe-close\nticket: none\nopened: ") || !strings.Contains(brief, "status: open\n---\n") ||
		!strings.Contains(brief, "Keep all artifacts") || !strings.HasPrefix(f.read(filepath.Join(task, "STATE.md")), "# State\n") {
		t.Fatalf("task folder\n%s", brief)
	}
	var tab *rexWindow
	for _, window := range loadRexAt(f).Sessions[0].Windows {
		if window.Label == "task:safe-close" {
			tab = window
		}
	}
	if tab == nil {
		t.Fatal("no task:safe-close tab")
	}
	f.fails(f.agent("task", "new", "garden", "--slug", "safe-close", "Again"), "never reused")
	f.fails(f.agent("task", "new", "garden", "--slug", "../escape", "Bad"), "Usage")
	f.ok(f.agent("task", "ticket", "garden", "safe-close", "GARDEN-12"))
	if !strings.Contains(f.read(filepath.Join(task, "BRIEF.md")), "ticket: GARDEN-12\n") {
		t.Fatal("ticket not attached")
	}
	f.fails(f.agent("task", "ticket", "garden", "safe-close", "GARDEN-13"), "already attached")
	if out := f.ok(f.agent("task", "ls", "garden")).stdout; !strings.Contains(out, "garden safe-close status=open ticket=GARDEN-12") {
		t.Fatalf("task ls %q", out)
	}

	worktree := filepath.Join(garden, ".worktrees/code/safe-close")
	f.write(filepath.Join(worktree, "uncommitted"), "must survive")
	f.write(".pi/agent/sessions/task.jsonl", "session history")
	f.write(filepath.Join(task, "draft.csv"), "irreplaceable data")
	kept := []string{filepath.Join(worktree, "uncommitted"), f.path(".pi/agent/sessions/task.jsonl"),
		filepath.Join(task, "draft.csv"), filepath.Join(task, "STATE.md")}
	before := map[string]string{}
	for _, path := range kept {
		before[path] = f.read(path)
	}

	f.ok(f.agent("task", "close", "garden", "safe-close"))
	if readJSON[map[string]any](t, meta)["status"] != "closing" {
		t.Fatal("close did not mark the task closing")
	}
	session := f.with("PI_PROJECT=garden", "PI_TASK=safe-close")
	f.fails(f.with("PI_PROJECT=garden").agent("task", "finalize", "garden", "safe-close"), "Only the task session")
	if r := session.agent("task", "finalize", "garden", "safe-close"); r.code == 0 {
		t.Fatalf("finalize without RESULT.md succeeded\n%s", r)
	}
	f.write(filepath.Join(task, "RESULT.md"), "# safe-close\n\n## Outcome\nVerified.\n")
	session.fails(session.agent("task", "finalize", "garden", "safe-close"), "headings")
	if f.exists(filepath.Join(garden, ".agent/tasks/safe-close/closed")) {
		t.Fatal("a failed finalize closed the task")
	}
	result := "# safe-close\n\n"
	for _, heading := range []string{"Outcome", "Changes", "Verification", "Learned", "Open"} {
		result += "## " + heading + "\nVerified.\n"
	}
	f.write(filepath.Join(task, "RESULT.md"), result)
	f.ok(session.agent("task", "finalize", "garden", "safe-close"))

	inbox := filepath.Join(garden, ".agent/inbox")
	folds := jsonFiles(t, inbox)
	if len(folds) != 1 || folds[0]["kind"] != "fold" || folds[0]["status"] != "new" || folds[0]["task"] != "safe-close" ||
		folds[0]["from"] != "garden" || folds[0]["to"] != "garden" {
		t.Fatalf("fold %v", folds)
	}
	// Closing again is idempotent and keeps the owner's decision on the fold.
	folds[0]["status"] = "accepted"
	f.write(filepath.Join(inbox, folds[0]["id"].(string)+".json"), mustJSON(t, folds[0]))
	f.ok(f.agent("task", "close", "garden", "safe-close"))
	if folds := jsonFiles(t, inbox); len(folds) != 1 || folds[0]["status"] != "accepted" {
		t.Fatalf("second close changed the fold %v", folds)
	}
	if readJSON[map[string]any](t, meta)["status"] != "closed" || !strings.Contains(f.read(filepath.Join(task, "BRIEF.md")), "status: closed\n") {
		t.Fatal("task not closed")
	}
	if f.read(filepath.Join(task, "RESULT.md")) != result {
		t.Fatal("RESULT.md changed")
	}
	for path, text := range before {
		if f.read(path) != text {
			t.Fatalf("%s changed", path)
		}
	}
	f.fails(f.agent("task", "restart", "garden", "safe-close"), "closed")
	if _, err := os.Stat(task); err != nil {
		t.Fatal(err)
	}
}

func TestTaskLimitAndTicketIdentity(t *testing.T) {
	f := newFixture(t, "hub")
	f.project("garden")["tasks"] = map[string]any{"maxOpen": 1}
	f.saveRegistry()
	f.ok(f.agent("task", "new", "garden", "GARDEN-1", "Ticket task"))
	brief := f.read(filepath.Join(f.workspace("garden"), "tasks/GARDEN-1/BRIEF.md"))
	if !strings.Contains(brief, "ticket: GARDEN-1\n") {
		t.Fatalf("ticket task brief\n%s", brief)
	}
	f.fails(f.agent("task", "new", "garden", "--slug", "second", "Over the limit"), "maxOpen=1")
	f.fails(f.agent("task", "new", "--slug", "orphan", "No project"), "")
	// Inside a project session the project may be omitted.
	f.ok(f.with("PI_PROJECT=garden").agent("task", "ls"))
}
