package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// writeTask builds a task's folders and metadata without the task commands, which a lifecycle
// test only needs as state.
func (f *fixture) writeTask(garden, id, status string) {
	f.t.Helper()
	f.write(filepath.Join(garden, "tasks", id, "BRIEF.md"), "# "+id+"\n")
	f.write(filepath.Join(garden, ".agent/tasks", id, "meta.json"),
		mustJSON(f.t, map[string]any{"id": id, "status": status}))
}

var restartStamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\n$`)

func TestArchiveRefusesOpenTasksAndRecordsLifecycle(t *testing.T) {
	f := newFixture(t, "hub")
	garden := f.open("garden")
	f.writeTask(garden, "audit", "open")
	f.fails(f.agent("archive", "garden"), "close open tasks before archiving")
	if f.exists(".local/state/agents/lifecycle/garden") {
		t.Fatal("a refused archive changed the lifecycle")
	}
	// A closed task permits the archive, which retains every task file.
	f.writeTask(garden, "audit", "closed")
	f.ok(f.agent("archive", "garden"))
	if f.read(".local/state/agents/lifecycle/garden") != "archived\n" ||
		f.read(filepath.Join(garden, "tasks/audit/BRIEF.md")) != "# audit\n" {
		t.Fatal("archive did not record the lifecycle or removed task files")
	}
	// An archived project refuses to start again.
	f.fails(f.agent("garden"), "archived, not starting")
}

func TestRestartAllMarksOnlyRunningTasksAndProjects(t *testing.T) {
	f := newFixture(t, "hub")
	garden := f.open("garden")
	f.writeTask(garden, "running", "open")
	f.writeTask(garden, "idle", "open")
	releaseProject := f.lock(filepath.Join(garden, ".agent/run.lock"))
	releaseTask := f.lock(filepath.Join(garden, ".agent/tasks/running/run.lock"))
	out := f.ok(f.agent("restart", "all")).stdout
	if !strings.Contains(out, "restart requested: garden") {
		t.Fatalf("restart all output\n%s", out)
	}
	if stamp := f.read(filepath.Join(garden, ".agent/restart-requested")); !restartStamp.MatchString(stamp) {
		t.Fatalf("project restart marker %q", stamp)
	}
	if stamp := f.read(filepath.Join(garden, ".agent/tasks/running/restart-requested")); !restartStamp.MatchString(stamp) {
		t.Fatalf("task restart marker %q", stamp)
	}
	if f.exists(filepath.Join(garden, ".agent/tasks/idle/restart-requested")) {
		t.Fatal("restart all marked an idle task")
	}
	releaseTask()
	releaseProject()
	// Without a runner no project is a restart target.
	if out := f.ok(f.agent("restart", "all")).stdout; !strings.Contains(out, "no running project agents") {
		t.Fatalf("restart all without runners\n%s", out)
	}
}

func TestCapacityCountsProjectAndTaskRunners(t *testing.T) {
	f := newFixture(t, "hub")
	f.registry["hosts"].(map[string]any)["hub"].(map[string]any)["maxAgents"] = 1
	f.saveRegistry()
	garden := f.open("garden")
	f.writeTask(garden, "audit", "open")
	releaseTask := f.lock(filepath.Join(garden, ".agent/tasks/audit/run.lock"))
	f.fails(f.agent("admin"), "maxAgents=1, stop an agent first")
	releaseTask()
	releaseProject := f.lock(filepath.Join(garden, ".agent/run.lock"))
	f.fails(f.agent("admin"), "maxAgents=1, stop an agent first")
	releaseProject()
}

func TestRegisterRefusesReservedExistingAndSymlinkedMembers(t *testing.T) {
	f := newFixture(t, "hub")
	garden := f.open("garden")
	f.ok(f.internal("--register", "garden", "local"))
	f.fails(f.internal("--register", "garden", "tasks"), "reserved")
	f.fails(f.internal("--register", "garden", "local"), "already registered")
	outside := filepath.Join(f.dir, "outside")
	f.git(f.dir, "init", "--initial-branch=main", outside)
	link := filepath.Join(garden, "linked")
	must(t, os.Symlink(outside, link))
	f.fails(f.internal("--register", "garden", "linked"), "escapes workspace")
	if target, err := os.Readlink(link); err != nil || target != outside {
		t.Fatalf("register changed the symlink: %q %v", target, err)
	}
}

func TestRestoreRefusesSymlinkedMemberPath(t *testing.T) {
	f := newFixture(t, "hub")
	_, remote := f.bareRemote()
	f.project("garden")["repos"] = []any{map[string]any{"name": "code", "url": remote}}
	f.saveRegistry()
	garden := f.open("garden")
	must(t, os.RemoveAll(filepath.Join(garden, "code")))
	outside := filepath.Join(f.dir, "outside")
	f.git(f.dir, "init", "--initial-branch=main", outside)
	f.write(filepath.Join(outside, "keep.txt"), "private\n")
	link := filepath.Join(garden, "code")
	must(t, os.Symlink(outside, link))
	f.fails(f.agent("garden"), "escapes workspace")
	if f.read(filepath.Join(outside, "keep.txt")) != "private\n" {
		t.Fatal("restore wrote into the symlink target")
	}
	if target, err := os.Readlink(link); err != nil || target != outside {
		t.Fatalf("restore changed the symlink: %q %v", target, err)
	}
}

func TestUpOpensEveryActiveProject(t *testing.T) {
	f := newFixture(t, "hub")
	f.project("garden")["lifecycle"] = "archived"
	f.saveRegistry()
	f.ok(f.agent("up"))
	if f.exists(f.workspace("garden")) {
		t.Fatal("up started an archived project")
	}
	if !f.exists(filepath.Join(f.workspace("admin"), "notes/PROJECT.md")) {
		t.Fatal("up did not open the active project")
	}
	f.fails(f.agent("up", "extra"), "Usage")
}

func TestLaunchPrefixedDispatchMatchesBareCommands(t *testing.T) {
	f := newFixture(t, "hub")
	f.ok(f.internal("--launch", "garden"))
	if !f.exists(filepath.Join(f.workspace("garden"), "notes/PROJECT.md")) {
		t.Fatal("--launch <project> did not open the project")
	}
	f.ok(f.internal("--launch", "open", "admin"))
	if !f.exists(filepath.Join(f.workspace("admin"), "notes/PROJECT.md")) {
		t.Fatal("--launch open <project> did not open the project")
	}
	f.ok(f.internal("--launch", "up"))
	f.fails(f.internal("--launch", "restart", "nobody"), "Unknown project")
}

func TestOpenLaunchesTheLegacyRunnerInItsPiTab(t *testing.T) {
	f := newFixture(t, "hub")
	f.open("garden")
	for _, c := range f.calls("rex") {
		if len(c.Args) < 4 || c.Args[0] != "api" || c.Args[2] != "session.create" {
			continue
		}
		var body struct {
			InitialWindows []struct {
				Layout struct {
					Block struct {
						Options struct {
							Shell   string   `json:"shell"`
							Command []string `json:"command"`
						} `json:"options"`
					} `json:"block"`
				} `json:"layout"`
			} `json:"initial_windows"`
		}
		must(t, json.Unmarshal([]byte(c.Args[3]), &body))
		options := body.InitialWindows[0].Layout.Block.Options
		want := []string{"fish", "-lc", "exec python3 " + f.path(".local/bin/agent-run") + " garden"}
		if implementation.kind != "python" {
			want = []string{"fish", "-lc", "exec " + f.path(".local/bin/agent") + " --run garden"}
		}
		if options.Shell != "none" || !reflect.DeepEqual(options.Command, want) {
			t.Fatalf("pi tab command %q shell %q", options.Command, options.Shell)
		}
		return
	}
	t.Fatal("open did not create a session")
}

func TestOpenOrdersCoordinatorFirstAndSelectsTheSingleApp(t *testing.T) {
	f := newFixture(t, "hub")
	state := loadRexAt(f)
	state.Sessions = []*rexSession{
		{ID: "session:other", Label: "garden", Windows: []*rexWindow{}},
		{ID: "session:coordinator", Label: "admin", Windows: []*rexWindow{}},
	}
	state.Next = 10
	state.Clients = []any{map[string]any{
		"client_id": "client:app", "connection_state": "connected",
		"info": map[string]any{"kind": "app"},
	}}
	state.saveAt(f)
	garden := f.open("garden")
	var moves []map[string]any
	for _, c := range f.calls("rex") {
		if len(c.Args) > 2 && c.Args[0] == "api" && c.Args[2] == "session.move" {
			var body map[string]any
			must(t, json.Unmarshal([]byte(c.Args[3]), &body))
			moves = append(moves, body)
		}
	}
	if len(moves) != 1 || moves[0]["session_id"] != "session:coordinator" ||
		moves[0]["before_session_id"] != "session:other" {
		t.Fatalf("session moves %v", moves)
	}
	var selected []string
	for _, c := range f.calls("rex") {
		if len(c.Args) > 2 && c.Args[0] == "-C" && c.Args[1] == "client:app" && c.Args[2] == "do" {
			selected = c.Args
		}
	}
	window := strings.TrimSpace(f.read(filepath.Join(garden, ".agent/rex-window")))
	if len(selected) == 0 || !contains(selected, "session_id=session:other") ||
		!contains(selected, "window_id="+window) {
		t.Fatalf("session select %q for window %q", selected, window)
	}
}

func TestArchiveLeavesLiveUnmanagedProcessAlone(t *testing.T) {
	f := newFixture(t, "hub")
	f.open("garden")
	state := loadRexAt(f)
	block := state.Sessions[0].Windows[0].Layers[0].Blocks[0].ID
	state.Process = map[string]any{block: map[string]any{
		"child":      map[string]any{"name": "vim", "pid": 7},
		"foreground": map[string]any{"pid": 7}}}
	state.saveAt(f)
	f.fails(f.agent("archive", "garden"), "live unmanaged process or unknown runner PID, not archiving")
	if f.exists(".local/state/agents/lifecycle/garden") {
		t.Fatal("a refused archive changed the lifecycle")
	}
}
