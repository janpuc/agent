package tests

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// skipWithoutScratch skips the scratch tests when the implementation under test has no scratch
// command. The suite still runs them against the Go command.
func skipWithoutScratch(t *testing.T) {
	t.Helper()
	if implementation.kind == "python" {
		t.Skip("the Python runner has no scratch command")
	}
}

// scratchID is the task id in a scratch new "opened" line.
var scratchID = regexp.MustCompile(`^workbench: task (\S+) opened\n$`)

// newScratch opens a generated scratch task and returns its id.
func (f *fixture) newScratch(brief string) string {
	f.t.Helper()
	match := scratchID.FindStringSubmatch(f.ok(f.agent("scratch", "new", brief)).stdout)
	if match == nil {
		f.t.Fatalf("scratch new did not print the opened line")
	}
	return match[1]
}

func TestScratchNewOpensTasksWithSlugOrGeneratedIdentity(t *testing.T) {
	skipWithoutScratch(t)
	f := newFixture(t, "hub")
	workbench := f.workspace("workbench")
	f.ok(f.agent("scratch", "new", "--slug", "tidy", "Tidy the notes"))
	brief := f.read(filepath.Join(workbench, "tasks/tidy/BRIEF.md"))
	if !strings.HasPrefix(brief, "---\nid: tidy\nticket: none\nopened: ") ||
		!strings.Contains(brief, "status: open\n---\n\nTidy the notes\n") {
		t.Fatalf("scratch task files\n%s", brief)
	}
	if readJSON[map[string]any](t, filepath.Join(workbench, ".agent/tasks/tidy/meta.json"))["status"] != "open" {
		t.Fatal("scratch task not open")
	}
	// Without --slug the id is the UTC minute, and a second task in that minute takes a suffix.
	generated := regexp.MustCompile(`^(s-[0-9]{6}-[0-9]{4})(-[0-9]+)?$`)
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = f.newScratch("Generated task " + strconv.Itoa(i))
	}
	for _, id := range ids {
		if !generated.MatchString(id) {
			t.Fatalf("generated id %q", id)
		}
		if !f.exists(filepath.Join(workbench, "tasks", id, "BRIEF.md")) {
			t.Fatalf("generated id %q has no task folder", id)
		}
	}
	if readJSON[map[string]any](t, filepath.Join(workbench, ".agent/tasks", ids[0], "meta.json"))["status"] != "open" {
		t.Fatal("generated scratch task not open")
	}
	if ids[0] == ids[1] || ids[1] == ids[2] || ids[0] == ids[2] {
		t.Fatalf("scratch ids repeated: %v", ids)
	}
	base := func(id string) string { return generated.FindStringSubmatch(id)[1] }
	if base(ids[0]) == base(ids[1]) && ids[1] != base(ids[0])+"-2" {
		t.Fatalf("first collision %v", ids)
	}
	if base(ids[0]) == base(ids[2]) && ids[2] != base(ids[0])+"-3" {
		t.Fatalf("second collision %v", ids)
	}
	f.fails(f.agent("scratch", "new", "--slug", "../escape", "Bad"), "Usage: agent scratch new")
	f.fails(f.agent("scratch", "new", "--slug", "blank", "  "), "A task needs a brief")
	f.fails(f.agent("scratch", "new"), "Usage: agent scratch new")
}

func TestScratchListOpenAndRestart(t *testing.T) {
	skipWithoutScratch(t)
	f := newFixture(t, "hub")
	f.ok(f.agent("scratch", "new", "--slug", "draft", "Draft the note"))
	f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	out := f.ok(f.agent("scratch", "ls")).stdout
	if out != "workbench draft status=open ticket=none pi=no host=hub\n" {
		t.Fatalf("scratch ls\n%s", out)
	}
	f.fails(f.agent("scratch", "ls", "extra"), "Usage: agent scratch ls")
	// The bare command opens the scratch project like agent <project>.
	f.ok(f.agent("scratch"))
	workbench := f.workspace("workbench")
	if !f.exists(filepath.Join(workbench, "notes/PROJECT.md")) ||
		!f.exists(filepath.Join(workbench, ".agent/rex-window")) {
		t.Fatal("scratch did not open the scratch project")
	}
	release := f.lock(filepath.Join(workbench, ".agent/tasks/draft/run.lock"))
	f.ok(f.agent("scratch", "restart", "draft"))
	if stamp := f.read(filepath.Join(workbench, ".agent/tasks/draft/restart-requested")); !restartStamp.MatchString(stamp) {
		t.Fatalf("scratch restart marker %q", stamp)
	}
	release()
	f.fails(f.agent("scratch", "restart", "nobody"), "no such file")
	f.fails(f.agent("scratch", "restart"), "Usage: agent scratch restart")
}

func TestScratchFoldRecordsTheTargetAndSendsOneFold(t *testing.T) {
	skipWithoutScratch(t)
	f := newFixture(t, "hub")
	f.ok(f.agent("scratch", "new", "--slug", "tidy", "Tidy the notes"))
	workbench := f.workspace("workbench")
	meta := filepath.Join(workbench, ".agent/tasks/tidy/meta.json")
	// Only a registered, active project on this host other than the scratch project may fold.
	f.fails(f.agent("scratch", "fold", "tidy", "nobody"), "Unknown project: nobody")
	f.fails(f.agent("scratch", "fold", "tidy", "field"), "Unknown project: field")
	f.fails(f.agent("scratch", "fold", "tidy", "workbench"),
		"Cannot fold a scratch task into the scratch project")
	f.project("admin")["lifecycle"] = "archived"
	f.saveRegistry()
	f.fails(f.agent("scratch", "fold", "tidy", "admin"), "admin: archived, not taking requests")
	f.project("admin")["lifecycle"] = "active"
	f.saveRegistry()
	if f.exists(filepath.Join(workbench, ".agent/tasks/tidy/close-requested")) {
		t.Fatal("a refused fold requested a close")
	}
	f.ok(f.agent("scratch", "fold", "tidy", "garden"))
	if recorded := readJSON[map[string]any](t, meta); recorded["foldTo"] != "garden" || recorded["status"] != "closing" {
		t.Fatalf("fold metadata %v", recorded)
	}
	f.fails(f.agent("scratch", "fold", "tidy", "admin"), "tidy: foldTo is already garden")
	f.write(filepath.Join(workbench, "tasks/tidy/RESULT.md"), "# tidy\n\n"+
		"## Outcome\nDone.\n## Changes\nNone.\n## Verification\nTests.\n## Learned\nNothing.\n## Open\nNone.\n")
	session := f.with("PI_PROJECT=workbench", "PI_TASK=tidy")
	f.ok(session.agent("task", "finalize", "workbench", "tidy"))
	gardenInbox := filepath.Join(f.workspace("garden"), ".agent/inbox")
	folds := jsonFiles(t, gardenInbox)
	want := "Take scratch task tidy from " + filepath.Join(workbench, "tasks/tidy/RESULT.md") +
		": copy what belongs into your workspace, fold RESULT.md into notes/PROJECT.md and " +
		"notes/STATE.md, and leave the scratch folder in place. Do not assume pending approvals were granted."
	if len(folds) != 1 || folds[0]["kind"] != "fold" || folds[0]["from"] != "workbench" ||
		folds[0]["to"] != "garden" || folds[0]["task"] != "tidy" || folds[0]["status"] != "new" ||
		folds[0]["text"] != want {
		t.Fatalf("fold %v", folds)
	}
	if stray := jsonFiles(t, filepath.Join(workbench, ".agent/inbox")); len(stray) != 0 {
		t.Fatalf("the scratch project received a fold %v", stray)
	}
	if readJSON[map[string]any](t, meta)["status"] != "closed" {
		t.Fatal("folded task not closed")
	}
	// Finalizing again writes no second fold, and a closed task refuses another fold.
	f.ok(session.agent("task", "finalize", "workbench", "tidy"))
	if again := jsonFiles(t, gardenInbox); len(again) != 1 || again[0]["id"] != folds[0]["id"] {
		t.Fatalf("second finalize wrote another fold %v", again)
	}
	f.fails(f.agent("scratch", "fold", "tidy", "admin"), "tidy: closed")
}

func TestScratchPromoteAsksTheCoordinatorToRegisterAProject(t *testing.T) {
	skipWithoutScratch(t)
	f := newFixture(t, "hub")
	f.ok(f.agent("scratch", "new", "--slug", "tidy", "Tidy the notes"))
	workbench := f.workspace("workbench")
	registry := f.read(".config/agents/projects.json")
	out := f.ok(f.agent("scratch", "promote", "tidy", "blueprint")).stdout
	id := strings.TrimSpace(out)
	if !regexp.MustCompile(`^[0-9]{8}T[0-9]{6}-[0-9a-f]{6}$`).MatchString(id) {
		t.Fatalf("promote printed %q", out)
	}
	items := jsonFiles(t, filepath.Join(f.workspace("admin"), ".agent/inbox"))
	want := "Register new project blueprint on hub from scratch task tidy (" +
		filepath.Join(workbench, "tasks/tidy") + "). Then run agent scratch fold tidy blueprint " +
		"so its agent takes the content in."
	if len(items) != 1 || items[0]["id"] != id || items[0]["from"] != "workbench" ||
		items[0]["to"] != "admin" || items[0]["status"] != "new" || items[0]["text"] != want {
		t.Fatalf("promote request %v", items)
	}
	if _, ok := items[0]["kind"]; ok {
		t.Fatalf("promote request has a kind %v", items[0])
	}
	// Promote changes nothing else.
	if f.read(".config/agents/projects.json") != registry {
		t.Fatal("promote changed the registry")
	}
	if meta := readJSON[map[string]any](t, filepath.Join(workbench, ".agent/tasks/tidy/meta.json")); meta["foldTo"] != nil {
		t.Fatalf("promote changed the task %v", meta)
	}
	if stray := jsonFiles(t, filepath.Join(workbench, ".agent/inbox")); len(stray) != 0 {
		t.Fatalf("promote wrote to the scratch project %v", stray)
	}
	f.fails(f.agent("scratch", "promote", "tidy", "admin"), "admin: already registered")
	f.fails(f.agent("scratch", "promote", "tidy", "../escape"), "Invalid project name")
	f.fails(f.agent("scratch", "promote", "nobody", "blueprint"), "no such file")
	f.fails(f.agent("scratch", "promote", "tidy"), "Usage: agent scratch promote")
}

func TestScratchRefusesAHostWithoutAScratchProject(t *testing.T) {
	skipWithoutScratch(t)
	f := newFixture(t, "outpost")
	for _, args := range [][]string{
		{"scratch"},
		{"scratch", "new", "Anything"},
		{"scratch", "ls"},
	} {
		f.fails(f.agent(args...), "outpost: no scratch project configured")
	}
	if f.exists(f.workspace("field")) || len(f.calls("rex")) != 0 {
		t.Fatal("a refused scratch command opened something")
	}
}

func TestScratchIsAReservedProjectName(t *testing.T) {
	skipWithoutScratch(t)
	f := newFixture(t, "hub")
	f.registry["projects"].(map[string]any)["scratch"] = map[string]any{"host": "hub", "lifecycle": "active"}
	f.saveRegistry()
	f.fails(f.agent("task", "ls"), "reserved")
	f.fails(f.agent("scratch"), "reserved")

	// The host's own scratch project may carry the name.
	f.registry["hosts"].(map[string]any)["hub"].(map[string]any)["scratchProject"] = "scratch"
	f.saveRegistry()
	f.ok(f.agent("task", "ls"))
}

func TestScratchOnAClientRunsOnTheHub(t *testing.T) {
	skipWithoutScratch(t)
	f := newFixture(t, "laptop")
	f.ok(f.agent("scratch", "it's a try"))
	f.ok(f.agent("scratch", "fold", "s-1", "garden"))
	calls := f.calls("ssh")
	if len(calls) != 2 {
		t.Fatalf("ssh calls: %v", calls)
	}
	want := []string{"hub", `env -u PI_PROJECT fish -lc 'agent scratch '\''it'\''\'\'''\''s a try'\'''`}
	if strings.Join(calls[0].Args, "\n") != strings.Join(want, "\n") {
		t.Fatalf("first call %q", calls[0].Args)
	}
	if calls[1].Args[1] != "env -u PI_PROJECT fish -lc 'agent scratch fold s-1 garden'" {
		t.Fatalf("second call %q", calls[1].Args)
	}
}

func TestScratchShortFormsOpenTasks(t *testing.T) {
	skipWithoutScratch(t)
	f := newFixture(t, "hub")
	workbench := f.workspace("workbench")
	f.ok(f.agent("scratch", "--slug", "short", "Short form"))
	f.ok(f.agent("scratch", "Brief only"))
	if !f.exists(filepath.Join(workbench, "tasks/short/BRIEF.md")) {
		t.Fatal("scratch --slug without new opened nothing")
	}
	if !strings.Contains(f.ok(f.agent("scratch", "ls")).stdout, "workbench s-") {
		t.Fatal("a lone brief opened no generated task")
	}
}
