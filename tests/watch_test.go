package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// creates counts the sessions the rex stub was asked to create.
func (f *fixture) creates() (n int) {
	f.t.Helper()
	for _, c := range f.calls("rex") {
		if len(c.Args) > 2 && c.Args[0] == "api" && c.Args[2] == "session.create" {
			n++
		}
	}
	return n
}

func TestWatchStartsOnlyDeadActiveRunnersOnce(t *testing.T) {
	f := newFixture(t, "hub")
	admin := f.open("admin")
	f.project("garden")["lifecycle"] = "archived"
	f.saveRegistry()
	// A held runner lock is a live runner, so admin needs no start.
	release := f.lock(filepath.Join(admin, ".agent/run.lock"))
	defer release()

	dry := f.ok(f.agent("watch", "--dry-run")).stdout
	if !strings.Contains(dry, "workbench: runner dead, would start it") {
		t.Fatalf("dry run does not name the dead runner\n%s", dry)
	}
	if strings.Contains(dry, "admin:") || strings.Contains(dry, "garden:") {
		t.Fatalf("dry run touches a live or archived project\n%s", dry)
	}
	if f.creates() != 1 || f.exists(".local/state/agents/watch") {
		t.Fatal("the dry run changed something")
	}

	if out := f.ok(f.agent("watch")).stdout; !strings.Contains(out, "workbench: runner started") {
		t.Fatalf("watch did not start the dead runner\n%s", out)
	}
	if f.creates() != 2 || !f.exists(f.workspace("workbench")) {
		t.Fatal("watch did not open the dead project")
	}
	if f.exists(f.workspace("garden")) {
		t.Fatal("watch opened an archived project")
	}
	if log := f.read(".local/state/agents/watch.log"); !strings.Contains(log, "workbench: runner started") {
		t.Fatalf("watch log %q", log)
	}

	// The second pass inside the interval starts nothing, so a dead project cannot loop.
	if out := f.ok(f.agent("watch")).stdout; !strings.Contains(out, "inside the 10m0s interval") {
		t.Fatalf("watch ignored the start interval\n%s", out)
	}
	if f.creates() != 2 {
		t.Fatal("watch started the runner twice inside the interval")
	}
}

var sessionEscape = regexp.MustCompile(`[/\\:]`)

// sessionDir is the directory Pi keeps the sessions of the runs started in cwd.
func sessionDir(home, cwd string) string {
	return filepath.Join(home, ".pi/agent/sessions",
		"--"+sessionEscape.ReplaceAllString(strings.TrimLeft(cwd, `/\`), "-")+"--")
}

// stalledSession is a session file whose last turn ended on a provider stall, followed by an entry
// that is not a message, as a live session has.
const stalledSession = `{"type":"message","id":"a","message":{"role":"user","content":[{"type":"text","text":"work"}]}}
{"type":"message","id":"b","message":{"role":"assistant","stopReason":"error","errorMessage":"ACP stalled for 5m with no output"}}
{"type":"custom","customType":"memini-state","data":{"captured":[]}}
`

func TestWatchContinuesAStalledTurnOnce(t *testing.T) {
	f := newFixture(t, "hub")
	admin := f.open("admin")
	release := f.lock(filepath.Join(admin, ".agent/run.lock"))
	defer release()
	inbox := filepath.Join(admin, ".agent/inbox")
	queued := func() []string {
		entries, err := os.ReadDir(inbox)
		if err != nil {
			return nil
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}

	path := filepath.Join(sessionDir(f.home, admin), "2026-11-25T09-00-00-000Z_admin-1.jsonl")
	f.write(path, stalledSession)
	old := time.Now().Add(-10 * time.Minute)
	must(t, os.Chtimes(path, old, old))

	out := f.ok(f.agent("watch")).stdout
	if !strings.Contains(out, `admin: continued the project session after "ACP stalled for 5m with no output"`) {
		t.Fatalf("watch did not continue the stalled turn\n%s", out)
	}
	if len(queued()) != 1 {
		t.Fatalf("inbox holds %v, want one continue", queued())
	}
	text := f.read(filepath.Join(inbox, queued()[0]))
	if !strings.Contains(text, `"from": "admin"`) || !strings.Contains(text, "Platform watchdog") ||
		!strings.Contains(text, "ACP stalled for 5m with no output") {
		t.Fatalf("continue item %s", text)
	}

	// The same stalled turn is never continued twice, so a quiet session cannot loop.
	out = f.ok(f.agent("watch")).stdout
	if len(queued()) != 1 || strings.Contains(out, "continued") {
		t.Fatalf("watch continued the same turn again\n%s", out)
	}

	// A turn that stalled a moment ago is left alone, a later stall is continued once more.
	f.write(path, stalledSession[strings.Index(stalledSession, "\n")+1:]+`{"type":"x"}`+"\n")
	f.ok(f.agent("watch"))
	if len(queued()) != 1 {
		t.Fatalf("watch continued a fresh turn: %v", queued())
	}
	must(t, os.Chtimes(path, old, old))
	f.ok(f.agent("watch"))
	if len(queued()) != 2 {
		t.Fatalf("watch did not continue the later stall: %v", queued())
	}
}
func TestWatchContinuesAStalledTaskThroughItsMessages(t *testing.T) {
	f := newFixture(t, "hub")
	garden := f.open("garden")
	f.writeTask(garden, "audit", "open")
	release := f.lock(filepath.Join(garden, ".agent/run.lock"))
	defer release()
	releaseTask := f.lock(filepath.Join(garden, ".agent/tasks/audit/run.lock"))
	defer releaseTask()
	// A task session runs in the task's own folder, so Pi keeps it under that path.
	path := filepath.Join(sessionDir(f.home, filepath.Join(garden, "tasks/audit")),
		"2026-11-25T09-00-00-000Z_garden-task-audit.jsonl")
	f.write(path, stalledSession)
	old := time.Now().Add(-10 * time.Minute)
	must(t, os.Chtimes(path, old, old))

	if out := f.ok(f.agent("watch")).stdout; !strings.Contains(out, "garden: continued task audit after") {
		t.Fatalf("watch did not continue the stalled task\n%s", out)
	}
	box := filepath.Join(garden, ".agent/tasks/audit/messages")
	entries, err := os.ReadDir(box)
	if err != nil || len(entries) != 1 {
		t.Fatalf("task messages %v %v", entries, err)
	}
	if text := f.read(filepath.Join(box, entries[0].Name())); !strings.Contains(text, "Platform watchdog") {
		t.Fatalf("task continue %s", text)
	}
}
