package tests

import (
	"path/filepath"
	"strings"
	"testing"
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