package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// taskWindow finds a task's Rex tab.
func taskWindow(t *testing.T, f *fixture, label string) *rexWindow {
	t.Helper()
	for _, session := range loadRexAt(f).Sessions {
		for _, window := range session.Windows {
			if window.Label == label {
				return window
			}
		}
	}
	t.Fatalf("no Rex window labelled %s", label)
	return nil
}

func TestTaskListCoversEveryProjectAndFallsBackToPIProject(t *testing.T) {
	f := newFixture(t, "hub")
	f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	f.ok(f.agent("task", "new", "admin", "ADMIN-7", "Review access"))
	out := f.ok(f.agent("task", "ls")).stdout
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 ||
		!contains(lines, "admin ADMIN-7 status=open ticket=ADMIN-7 pi=no host=hub") ||
		!contains(lines, "garden audit status=open ticket=none pi=no host=hub") {
		t.Fatalf("task ls all\n%s", out)
	}
	if strings.Contains(out, "field") {
		t.Fatalf("task ls listed another host's project\n%s", out)
	}
	garden := f.workspace("garden")
	out = f.ok(f.agent("task", "ls", "garden")).stdout
	if strings.Contains(out, "admin") || !strings.Contains(out, "garden audit status=open ticket=none") {
		t.Fatalf("task ls garden\n%s", out)
	}
	// ls without a project argument ignores PI_PROJECT and lists the whole host.
	if out := f.with("PI_PROJECT=admin").agent("task", "ls").stdout; !strings.Contains(out, "admin ADMIN-7") ||
		!strings.Contains(out, "garden audit") {
		t.Fatalf("task ls in a project session\n%s", out)
	}
	// Other task commands take the project from PI_PROJECT when it is omitted.
	f.ok(f.with("PI_PROJECT=garden").agent("task", "ticket", "audit", "GARDEN-12"))
	brief := f.read(filepath.Join(garden, "tasks/audit/BRIEF.md"))
	if !strings.Contains(brief, "ticket: GARDEN-12\n") {
		t.Fatalf("ticket from PI_PROJECT\n%s", brief)
	}
	f.ok(f.agent("task", "ticket", "garden", "audit", "GARDEN-12"))
	f.fails(f.agent("task", "ticket", "garden", "audit", "GARDEN-13"), "already attached")
	f.fails(f.agent("task", "ticket", "garden", "audit", "../escape"), "Invalid ticket identity")
	release := f.lock(filepath.Join(garden, ".agent/tasks/audit/run.lock"))
	if out := f.ok(f.agent("task", "ls", "garden")).stdout; !strings.Contains(out, "pi=yes") {
		t.Fatalf("live task runner not shown\n%s", out)
	}
	release()
	f.fails(f.agent("task", "ls", "nobody"), "Unknown project")
	f.fails(f.agent("task", "ls", "garden", "extra"), "Usage: agent task ls [project]")
}

func TestTaskStartsTheTaskRunnerInItsOwnTab(t *testing.T) {
	f := newFixture(t, "hub")
	f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	garden := f.workspace("garden")
	target := filepath.Join(garden, "tasks/audit")
	brief := f.read(filepath.Join(target, "BRIEF.md"))
	opened := regexp.MustCompile(`(?m)^opened: (.+)$`).FindStringSubmatch(brief)
	if opened == nil {
		t.Fatalf("brief front matter\n%s", brief)
	}
	wantBrief := "---\nid: audit\nticket: none\nopened: " + opened[1] + "\nstatus: open\n---\n\nAudit the speakers\n"
	wantMeta := "{\n  \"id\": \"audit\",\n  \"ticket\": null,\n  \"opened\": \"" + opened[1] +
		"\",\n  \"status\": \"open\"\n}\n"
	if brief != wantBrief || f.read(filepath.Join(garden, ".agent/tasks/audit/meta.json")) != wantMeta {
		t.Fatalf("task files\nbrief: %q\nmeta: %q", brief,
			f.read(filepath.Join(garden, ".agent/tasks/audit/meta.json")))
	}
	var created *call
	for _, c := range f.calls("rex") {
		if len(c.Args) > 4 && c.Args[0] == "window" && c.Args[1] == "new" && c.Args[4] == "task:audit" {
			call := c
			created = &call
		}
	}
	if created == nil {
		t.Fatal("task new did not create a task:audit tab")
	}
	want := "exec python3 " + f.path(".local/bin/agent-run") + " --task-run garden audit"
	if flag(created.Args, "-c") != target || lastArg(*created) != want ||
		!contains(created.Args, "--focus=false") || !contains(created.Args, "--shell=none") ||
		!contains(created.Args, "--keep-open") {
		t.Fatalf("task tab %q", created.Args)
	}
	first := taskWindow(t, f, "task:audit").Layers[0].Blocks[0].ID
	f.ok(f.agent("task", "close", "garden", "audit"))
	closing := regexp.MustCompile(`(?s)^\{\n  "id": "audit",\n  "ticket": null,\n  "opened": "` +
		regexp.QuoteMeta(opened[1]) + `",\n  "status": "closing",\n  "foldRequest": "[0-9]{8}T[0-9]{6}-[0-9a-f]{6}"\n\}\n$`)
	if meta := f.read(filepath.Join(garden, ".agent/tasks/audit/meta.json")); !closing.MatchString(meta) {
		t.Fatalf("closing metadata\n%s", meta)
	}
	window := taskWindow(t, f, "task:audit")
	if len(window.Layers[0].Blocks) != 1 || window.Layers[0].Blocks[0].ID == first {
		t.Fatalf("task tab not reused: %+v", window)
	}
	if !f.exists(filepath.Join(garden, ".agent/tasks/audit/close-requested")) {
		t.Fatal("close did not request the result")
	}
}

func TestTaskRestartLeavesLiveProcessesAndAmbiguousTabsAlone(t *testing.T) {
	f := newFixture(t, "hub")
	f.ok(f.agent("task", "new", "garden", "--slug", "probe", "Probe the tab"))
	garden := f.workspace("garden")
	block := taskWindow(t, f, "task:probe").Layers[0].Blocks[0].ID
	state := loadRexAt(f)
	state.Process = map[string]any{block: map[string]any{
		"child":      map[string]any{"name": "vim", "pid": 7},
		"foreground": map[string]any{"pid": 7}}}
	state.saveAt(f)
	f.fails(f.agent("task", "restart", "garden", "probe"), "probe: unmanaged process, leaving it alone")
	state = loadRexAt(f)
	state.Process = map[string]any{}
	for _, window := range state.Sessions[0].Windows {
		if window.Label == "task:probe" {
			window.Layers[0].Blocks = append(window.Layers[0].Blocks, rexBlock{ID: "block:extra"})
		}
	}
	state.saveAt(f)
	f.fails(f.agent("task", "restart", "garden", "probe"), "probe: ambiguous tab, leaving it alone")
	if !f.exists(filepath.Join(garden, "tasks/probe/BRIEF.md")) {
		t.Fatal("a refused restart removed the task")
	}
}

func TestTaskRestartMarksLiveSessionsAndRefusesClosedTasks(t *testing.T) {
	f := newFixture(t, "hub")
	f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	garden := f.workspace("garden")
	state := filepath.Join(garden, ".agent/tasks/audit")
	release := f.lock(filepath.Join(state, "run.lock"))
	f.ok(f.agent("task", "restart", "garden", "audit"))
	if stamp := f.read(filepath.Join(state, "restart-requested")); !restartStamp.MatchString(stamp) {
		t.Fatalf("task restart marker %q", stamp)
	}
	release()
	f.ok(f.agent("task", "close", "garden", "audit"))
	f.write(filepath.Join(garden, "tasks/audit/RESULT.md"), "# audit\n\n"+
		"## Outcome\nVerified.\n## Changes\nNone.\n## Verification\nTests.\n## Learned\nNothing.\n## Open\nNone.\n")
	session := f.with("PI_PROJECT=garden", "PI_TASK=audit")
	f.ok(session.agent("task", "finalize", "garden", "audit"))
	must(t, os.Remove(filepath.Join(state, "restart-requested")))
	f.fails(f.agent("task", "restart", "garden", "audit"), "audit: closed")
	if f.exists(filepath.Join(state, "restart-requested")) {
		t.Fatal("a refused restart wrote a marker")
	}
}

func TestTaskFinalizeRunsAgainWithoutASecondFold(t *testing.T) {
	f := newFixture(t, "hub")
	f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	garden := f.workspace("garden")
	f.ok(f.agent("task", "close", "garden", "audit"))
	f.write(filepath.Join(garden, "tasks/audit/RESULT.md"), "# audit\n\n"+
		"## Outcome\nVerified.\n## Changes\nNone.\n## Verification\nTests.\n## Learned\nNothing.\n## Open\nNone.\n")
	session := f.with("PI_PROJECT=garden", "PI_TASK=audit")
	f.ok(session.agent("task", "finalize", "garden", "audit"))
	inbox := filepath.Join(garden, ".agent/inbox")
	folds := jsonFiles(t, inbox)
	if len(folds) != 1 {
		t.Fatalf("folds %v", folds)
	}
	id := folds[0]["id"].(string)
	f.ok(session.agent("task", "finalize", "garden", "audit"))
	again := jsonFiles(t, inbox)
	if len(again) != 1 || again[0]["id"] != id || again[0]["kind"] != "fold" {
		t.Fatalf("second finalize wrote another fold %v", again)
	}
	if readJSON[map[string]any](t, filepath.Join(garden, ".agent/tasks/audit/meta.json"))["status"] != "closed" {
		t.Fatal("task not closed")
	}
}

func TestTaskMaxOpenComesFromDefaultsAndProjects(t *testing.T) {
	f := newFixture(t, "hub")
	for _, slug := range []string{"one", "two", "three", "four"} {
		f.ok(f.agent("task", "new", "garden", "--slug", slug, "Task "+slug))
	}
	f.fails(f.agent("task", "new", "garden", "--slug", "over", "Over the limit"),
		"garden: maxOpen=4, close an existing task first")
	f.project("admin")["tasks"] = map[string]any{"maxOpen": 2}
	f.saveRegistry()
	for i := 1; i <= 2; i++ {
		f.ok(f.agent("task", "new", "admin", "--slug", fmt.Sprintf("task-%d", i), "Task"))
	}
	f.fails(f.agent("task", "new", "admin", "--slug", "task-3", "Task"),
		"admin: maxOpen=2, close an existing task first")
	f.fails(f.agent("task", "new", "garden", "--slug", "blank", "  "), "A task needs a brief")
}

func TestLaunchPrefixedTaskCommandsMatchBareCommands(t *testing.T) {
	f := newFixture(t, "hub")
	f.ok(f.internal("--launch", "task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	if out := f.ok(f.internal("--launch", "task", "ls", "garden")).stdout; !strings.Contains(out, "garden audit status=open") {
		t.Fatalf("--launch task ls\n%s", out)
	}
	f.ok(f.internal("--launch", "task", "ticket", "garden", "audit", "GARDEN-3"))
	if brief := f.read(filepath.Join(f.workspace("garden"), "tasks/audit/BRIEF.md")); !strings.Contains(brief, "ticket: GARDEN-3\n") {
		t.Fatalf("--launch task ticket\n%s", brief)
	}
}
