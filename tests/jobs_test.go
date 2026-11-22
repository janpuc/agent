package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// jobFixture is a hub whose project garden has a member repository app with a build script, and
// a worker lab-mac with its own home behind the ssh stub.
func jobFixture(t *testing.T, script string) (*fixture, string) {
	t.Helper()
	f := newFixture(t, "hub")
	f.registry["platform"].(map[string]any)["workers"] = []any{"lab-mac"}
	f.saveRegistry()
	repo := filepath.Join(f.workspace("garden"), "app")
	must(t, os.MkdirAll(repo, 0o755))
	f.git(repo, "init", "-q")
	must(t, os.WriteFile(filepath.Join(repo, "build.sh"), []byte(script), 0o755))
	f.git(repo, "add", "build.sh")
	f.git(repo, "commit", "-q", "-m", "build")
	workerHome := filepath.Join(f.dir, "worker-home")
	must(t, os.MkdirAll(workerHome, 0o755))
	f.setWorker(false, false)
	return f, workerHome
}

func (f *fixture) setWorker(offline, dropJob bool) {
	f.t.Helper()
	f.write(filepath.Join(f.dir, "worker.json"), mustJSON(f.t, map[string]any{
		"name": "lab-mac", "home": filepath.Join(f.dir, "worker-home"), "agent": implementation.command[0],
		"offline": offline, "dropJob": dropJob,
	}))
}

var jobIDPattern = regexp.MustCompile(`Job ([0-9]{8}T[0-9]{6}-[0-9a-f]{6})`)

func TestJobWaitsForAnOfflineWorkerAndDeliversItsResult(t *testing.T) {
	if implementation.kind == "python" {
		t.Skip("jobs exist only in the Go agent")
	}
	f, workerHome := jobFixture(t, "#!/bin/sh\nmkdir -p out\necho \"built $1\" > out/app.txt\necho \"log line $1\"\n")
	f.setWorker(true, false)
	queued := f.ok(f.with("PI_PROJECT=garden").agent("job", "run", "lab-mac", "app", "--fetch", "out", "--", "./build.sh", "hello"))
	for _, want := range []string{"lab-mac is offline, which is normal", "Tell Jan once", "Do not retry, poll, or build on another machine"} {
		if !strings.Contains(queued.stdout, want) {
			t.Fatalf("queue message lacks %q\n%s", want, queued)
		}
	}
	match := jobIDPattern.FindStringSubmatch(queued.stdout)
	if match == nil {
		t.Fatalf("no job id\n%s", queued)
	}
	id := match[1]
	if out := f.ok(f.agent("job", "ls")).stdout; !strings.Contains(out, id+" lab-mac status=queued garden app@") {
		t.Fatalf("job ls\n%s", out)
	}
	// Draining while the worker is off changes nothing and delivers nothing.
	f.ok(f.agent("job", "--drain"))
	inbox := filepath.Join(f.workspace("garden"), ".agent/inbox")
	if f.exists(inbox) && len(jsonFiles(t, inbox)) != 0 {
		t.Fatal("a result arrived while the worker was off")
	}
	f.setWorker(false, false)
	f.ok(f.agent("job", "--drain"))
	job := readJSON[map[string]any](t, filepath.Join(f.home, ".local/state/agents/jobs", id, "job.json"))
	if job["status"] != "done" || job["exit"] != float64(0) || job["attempts"] != float64(1) {
		t.Fatalf("job after the drain %v", job)
	}
	artifact, err := os.ReadFile(filepath.Join(f.home, ".local/state/agents/jobs", id, "artifacts/out/app.txt"))
	if err != nil || string(artifact) != "built hello\n" {
		t.Fatalf("artifact %q %v", artifact, err)
	}
	items := jsonFiles(t, inbox)
	if len(items) != 1 || items[0]["inReplyTo"] != id || items[0]["from"] != "lab-mac" || items[0]["to"] != "garden" {
		t.Fatalf("inbox %v", items)
	}
	text := items[0]["text"].(string)
	for _, want := range []string{"finished, exit 0", "./build.sh hello", "log line hello", "Artifacts:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("result lacks %q\n%s", want, text)
		}
	}
	// The worker keeps its checkout, the member repository keeps no job ref.
	if _, err := os.Stat(filepath.Join(workerHome, "Builds/garden/app/build.sh")); err != nil {
		t.Fatal(err)
	}
	if refs := f.git(filepath.Join(f.workspace("garden"), "app"), "for-each-ref", "refs/agent-job"); strings.TrimSpace(refs) != "" {
		t.Fatalf("job ref left behind: %s", refs)
	}
	// A second drain finds nothing to do.
	f.ok(f.agent("job", "--drain"))
	if len(jsonFiles(t, inbox)) != 1 {
		t.Fatal("a finished job was delivered twice")
	}
}

func TestJobCutOffByTheWorkerWaitsAgain(t *testing.T) {
	if implementation.kind == "python" {
		t.Skip("jobs exist only in the Go agent")
	}
	f, _ := jobFixture(t, "#!/bin/sh\nexit 3\n")
	f.setWorker(true, false)
	id := jobIDPattern.FindStringSubmatch(f.ok(f.with("PI_PROJECT=garden").agent("job", "run", "lab-mac", "app", "--", "./build.sh")).stdout)[1]
	jobPath := filepath.Join(f.home, ".local/state/agents/jobs", id, "job.json")
	f.setWorker(false, true)
	f.ok(f.agent("job", "--drain"))
	job := readJSON[map[string]any](t, jobPath)
	if job["status"] != "queued" || job["attempts"] != float64(1) {
		t.Fatalf("a cut-off job must wait again %v", job)
	}
	inbox := filepath.Join(f.workspace("garden"), ".agent/inbox")
	if f.exists(inbox) && len(jsonFiles(t, inbox)) != 0 {
		t.Fatal("a cut-off job was reported")
	}
	// The job's own failure is a result like any other.
	f.setWorker(false, false)
	f.ok(f.agent("job", "--drain"))
	job = readJSON[map[string]any](t, jobPath)
	if job["status"] != "failed" || job["exit"] != float64(3) || job["attempts"] != float64(2) {
		t.Fatalf("job %v", job)
	}
	items := jsonFiles(t, inbox)
	if len(items) != 1 || !strings.Contains(items[0]["text"].(string), "finished, exit 3") {
		t.Fatalf("inbox %v", items)
	}
}

func TestJobCancelAndRefusals(t *testing.T) {
	if implementation.kind == "python" {
		t.Skip("jobs exist only in the Go agent")
	}
	f, _ := jobFixture(t, "#!/bin/sh\n")
	f.setWorker(true, false)
	in := f.with("PI_PROJECT=garden")
	f.fails(in.agent("job", "run", "other-mac", "app", "--", "make"), "unknown worker")
	f.fails(in.agent("job", "run", "lab-mac", "app", "--", "bash", "-c", "id"), "a job starts one of")
	f.fails(in.agent("job", "run", "lab-mac", "app", "--", "./../escape.sh"), "stay inside the repository")
	f.fails(in.agent("job", "run", "lab-mac", "app", "--fetch", "../x", "--", "make"), "stay inside the repository")
	f.fails(in.agent("job", "run", "lab-mac", "missing", "--", "make"), "is not a repository of garden")
	f.fails(in.agent("job", "run", "lab-mac", "app", "make"), "Usage: agent job run")
	f.fails(f.agent("job", "run", "lab-mac", "app", "--", "make"), "must run in a project session")
	id := jobIDPattern.FindStringSubmatch(f.ok(in.agent("job", "run", "lab-mac", "app", "--", "make")).stdout)[1]
	f.ok(f.agent("job", "cancel", id))
	f.fails(f.agent("job", "cancel", id), "only a queued job can be cancelled")
	f.setWorker(false, false)
	f.ok(f.agent("job", "--drain"))
	if job := readJSON[map[string]any](t, filepath.Join(f.home, ".local/state/agents/jobs", id, "job.json")); job["status"] != "cancelled" {
		t.Fatalf("a cancelled job ran %v", job)
	}
}

func TestWorkerEndpointAcceptsOnlyItsOperations(t *testing.T) {
	if implementation.kind == "python" {
		t.Skip("jobs exist only in the Go agent")
	}
	f := newFixture(t, "laptop")
	for _, operation := range []string{"", "bash", "ping; id", "work-cleanup", "work-cleanup --really --yes", "agent ls"} {
		f.fails(f.with("SSH_ORIGINAL_COMMAND="+operation).internal("--worker-endpoint"), "worker endpoint accepts only")
	}
	if out := f.ok(f.with("SSH_ORIGINAL_COMMAND=ping").internal("--worker-endpoint")).stdout; out != "ok\n" {
		t.Fatalf("ping %q", out)
	}
	spec := `{"id":"20261122T100000-abcdef","project":"garden","repo":"app","commit":"` + strings.Repeat("a", 40) + `","args":["bash","-c","id"]}` + "\n"
	f.fails(f.with("SSH_ORIGINAL_COMMAND=job").input(spec).internal("--worker-endpoint"), "a job starts one of")
	escape := `{"id":"20261122T100000-abcdef","project":"garden","repo":"..","commit":"` + strings.Repeat("a", 40) + `","args":["make"]}` + "\n"
	f.fails(f.with("SSH_ORIGINAL_COMMAND=job").input(escape).internal("--worker-endpoint"), "invalid repository name")
	fetch := `{"id":"20261122T100000-abcdef","project":"garden","repo":"app","path":"../../.ssh"}` + "\n"
	f.fails(f.with("SSH_ORIGINAL_COMMAND=fetch").input(fetch).internal("--worker-endpoint"), "stay inside the repository")
	if f.exists("Builds/garden") {
		t.Fatal("a refused job touched ~/Builds")
	}
}
