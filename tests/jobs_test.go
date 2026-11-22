package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// jobFixture is a hub whose project garden has a member repository app with an executable build
// script, and a worker lab-mac with its own home behind the ssh stub.
func jobFixture(t *testing.T, script string) (*fixture, string) {
	t.Helper()
	if implementation.kind == "python" {
		t.Skip("jobs exist only in the Go agent")
	}
	f := newFixture(t, "hub")
	f.registry["platform"].(map[string]any)["workers"] = []any{"lab-mac"}
	f.saveRegistry()
	repo := filepath.Join(f.open("garden"), "app")
	must(t, os.MkdirAll(repo, 0o755))
	f.git(repo, "init", "-q")
	must(t, os.WriteFile(filepath.Join(repo, "build.sh"), []byte(script), 0o755))
	must(t, os.WriteFile(filepath.Join(repo, "notes.sh"), []byte("#!/bin/sh\n"), 0o644))
	f.git(repo, "add", "build.sh", "notes.sh")
	f.git(repo, "commit", "-q", "-m", "build")
	workerHome := filepath.Join(f.dir, "worker-home")
	must(t, os.MkdirAll(workerHome, 0o755))
	f.setWorker(map[string]any{})
	return f, workerHome
}

// setWorker describes the worker for the ssh stub: offline, dropJob, dropFetch or refuse.
func (f *fixture) setWorker(state map[string]any) {
	f.t.Helper()
	state["name"], state["home"], state["agent"] = "lab-mac", filepath.Join(f.dir, "worker-home"), implementation.command[0]
	f.write(filepath.Join(f.dir, "worker.json"), mustJSON(f.t, state))
}

var jobIDPattern = regexp.MustCompile(`Job ([0-9]{8}T[0-9]{6}-[0-9a-f]{6})`)

// queueJob queues a job while the worker is off, so no drain starts in the background, then puts
// the worker in state.
func (f *fixture) queueJob(state map[string]any, args ...string) string {
	f.t.Helper()
	f.setWorker(map[string]any{"offline": true})
	defer f.setWorker(state)
	out := f.ok(f.with("PI_PROJECT=garden").agent(append([]string{"job", "run", "lab-mac", "app"}, args...)...))
	match := jobIDPattern.FindStringSubmatch(out.stdout)
	if match == nil {
		f.t.Fatalf("no job id\n%s", out)
	}
	return match[1]
}

func (f *fixture) job(id string) map[string]any {
	return readJSON[map[string]any](f.t, filepath.Join(f.home, ".local/state/agents/jobs", id, "job.json"))
}

func (f *fixture) gardenInbox() []map[string]any {
	inbox := filepath.Join(f.workspace("garden"), ".agent/inbox")
	if !f.exists(inbox) {
		return nil
	}
	return jsonFiles(f.t, inbox)
}

const countingScript = "#!/bin/sh\necho run >> \"$HOME/runs\"\nmkdir -p a b\necho \"a $1\" > a/x.txt\necho \"b $1\" > b/x.txt\necho \"log line $1\"\n"

func TestJobWaitsForAnOfflineWorkerAndDeliversItsResult(t *testing.T) {
	f, workerHome := jobFixture(t, countingScript)
	f.setWorker(map[string]any{"offline": true})
	queued := f.ok(f.with("PI_PROJECT=garden").agent("job", "run", "lab-mac", "app", "--fetch", "a/x.txt", "--fetch", "b", "--", "./build.sh", "hello"))
	for _, want := range []string{"lab-mac is offline, which is normal", "Tell Jan once", "Do not retry, poll, or build on another machine"} {
		if !strings.Contains(queued.stdout, want) {
			t.Fatalf("queue message lacks %q\n%s", want, queued)
		}
	}
	id := jobIDPattern.FindStringSubmatch(queued.stdout)[1]
	if out := f.ok(f.agent("job", "ls")).stdout; !strings.Contains(out, id+" lab-mac status=queued garden app@") {
		t.Fatalf("job ls\n%s", out)
	}
	if out := f.ok(f.agent("todo")).stdout; !strings.Contains(out, "jobs: job "+id+" for garden waits for lab-mac to be switched on") {
		t.Fatalf("todo\n%s", out)
	}
	// Draining while the worker is off changes nothing and delivers nothing.
	f.ok(f.agent("job", "--drain"))
	if len(f.gardenInbox()) != 0 {
		t.Fatal("a result arrived while the worker was off")
	}
	f.setWorker(map[string]any{})
	f.ok(f.agent("job", "--drain"))
	job := f.job(id)
	if job["status"] != "done" || job["exit"] != float64(0) || job["attempts"] != float64(1) {
		t.Fatalf("job after the drain %v", job)
	}
	// Both kept paths arrive under their checkout paths, the same base name does not collide.
	artifacts := filepath.Join(f.home, ".local/state/agents/jobs", id, "artifacts")
	for path, want := range map[string]string{"a/x.txt": "a hello\n", "b/x.txt": "b hello\n"} {
		if data, err := os.ReadFile(filepath.Join(artifacts, path)); err != nil || string(data) != want {
			t.Fatalf("artifact %s %q %v", path, data, err)
		}
	}
	items := f.gardenInbox()
	if len(items) != 1 || items[0]["inReplyTo"] != id || items[0]["from"] != "lab-mac" || items[0]["to"] != "garden" {
		t.Fatalf("inbox %v", items)
	}
	text := items[0]["text"].(string)
	for _, want := range []string{"finished, exit 0", "./build.sh hello", "log line hello", "Artifacts:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("result lacks %q\n%s", want, text)
		}
	}
	if refs := f.git(filepath.Join(f.workspace("garden"), "app"), "for-each-ref", "refs/agent-job"); strings.TrimSpace(refs) != "" {
		t.Fatalf("job ref left behind: %s", refs)
	}
	f.ok(f.agent("job", "--drain"))
	if len(f.gardenInbox()) != 1 {
		t.Fatal("a finished job was delivered twice")
	}
	if out := f.ok(f.agent("todo")).stdout; strings.Contains(out, "jobs:") {
		t.Fatalf("a finished job still waits in todo\n%s", out)
	}
	// A second job builds in the same checkout, which the worker created and keeps.
	if _, err := os.Stat(filepath.Join(workerHome, "Builds/garden/app/.git/agent-worker-checkout")); err != nil {
		t.Fatal(err)
	}
	second := f.queueJob(map[string]any{}, "--", "./build.sh", "again")
	f.ok(f.agent("job", "--drain"))
	if job := f.job(second); job["status"] != "done" {
		t.Fatalf("second job %v", job)
	}
}

func TestJobCutOffByTheWorkerWaitsAgainAndFetchesWithoutRebuilding(t *testing.T) {
	f, workerHome := jobFixture(t, countingScript)
	id := f.queueJob(map[string]any{"dropJob": true}, "--fetch", "a", "--", "./build.sh")
	f.ok(f.agent("job", "--drain"))
	if job := f.job(id); job["status"] != "queued" || job["attempts"] != float64(1) {
		t.Fatalf("a cut-off job must wait again %v", job)
	}
	if len(f.gardenInbox()) != 0 {
		t.Fatal("a cut-off job was reported")
	}
	// The build runs, then the connection drops while its files are fetched.
	f.setWorker(map[string]any{"dropFetch": true})
	f.ok(f.agent("job", "--drain"))
	if job := f.job(id); job["status"] != "fetching" || job["exit"] != float64(0) {
		t.Fatalf("a build cut off while fetching must wait to fetch %v", job)
	}
	f.setWorker(map[string]any{})
	f.ok(f.agent("job", "--drain"))
	if job := f.job(id); job["status"] != "done" {
		t.Fatalf("job %v", job)
	}
	if runs, _ := os.ReadFile(filepath.Join(workerHome, "runs")); string(runs) != "run\n" {
		t.Fatalf("the build ran %q, want once", runs)
	}
}

func TestJobOwnFailureIsAResult(t *testing.T) {
	f, _ := jobFixture(t, "#!/bin/sh\necho failing\nexit 3\n")
	id := f.queueJob(map[string]any{}, "--", "./build.sh")
	f.ok(f.agent("job", "--drain"))
	if job := f.job(id); job["status"] != "failed" || job["exit"] != float64(3) {
		t.Fatalf("job %v", job)
	}
	items := f.gardenInbox()
	if len(items) != 1 || !strings.Contains(items[0]["text"].(string), "finished, exit 3") {
		t.Fatalf("inbox %v", items)
	}
}

func TestJobOnAWorkerThatRefusesIsReportedOnceAndWaits(t *testing.T) {
	f, _ := jobFixture(t, "#!/bin/sh\n")
	f.setWorker(map[string]any{"refuse": "Host key verification failed."})
	out := f.ok(f.with("PI_PROJECT=garden").agent("job", "run", "lab-mac", "app", "--", "./build.sh"))
	if !strings.Contains(out.stdout, "refuses jobs: Host key verification failed.") || strings.Contains(out.stdout, "offline") {
		t.Fatalf("queue message\n%s", out)
	}
	id := jobIDPattern.FindStringSubmatch(out.stdout)[1]
	f.ok(f.agent("job", "--drain"))
	f.ok(f.agent("job", "--drain"))
	items := f.gardenInbox()
	if len(items) != 1 || !strings.Contains(items[0]["text"].(string), "Host key verification failed") || items[0]["inReplyTo"] != id {
		t.Fatalf("want one refusal notice, got %v", items)
	}
	if job := f.job(id); job["status"] != "queued" {
		t.Fatalf("a refused job must wait %v", job)
	}
	if out := f.ok(f.agent("todo")).stdout; !strings.Contains(out, "lab-mac refuses jobs: Host key verification failed.") {
		t.Fatalf("todo\n%s", out)
	}
	f.setWorker(map[string]any{})
	f.ok(f.agent("job", "--drain"))
	if job := f.job(id); job["status"] != "done" {
		t.Fatalf("job once fixed %v", job)
	}
}

func TestJobRefusesAForeignCheckout(t *testing.T) {
	f, workerHome := jobFixture(t, "#!/bin/sh\n")
	foreign := filepath.Join(workerHome, "Builds/garden/app")
	must(t, os.MkdirAll(foreign, 0o755))
	must(t, os.WriteFile(filepath.Join(foreign, "mine.txt"), []byte("Jan's file"), 0o644))
	id := f.queueJob(map[string]any{}, "--", "./build.sh")
	f.ok(f.agent("job", "--drain"))
	if job := f.job(id); job["status"] != "failed" {
		t.Fatalf("job %v", job)
	}
	if text := f.gardenInbox()[0]["text"].(string); !strings.Contains(text, "was not created by the worker endpoint") {
		t.Fatalf("result\n%s", text)
	}
	if data, err := os.ReadFile(filepath.Join(foreign, "mine.txt")); err != nil || string(data) != "Jan's file" {
		t.Fatalf("a foreign checkout was touched: %q %v", data, err)
	}
}

func TestJobCancelAndRefusals(t *testing.T) {
	f, _ := jobFixture(t, "#!/bin/sh\n")
	f.setWorker(map[string]any{"offline": true})
	in := f.with("PI_PROJECT=garden")
	f.fails(in.agent("job", "run", "other-mac", "app", "--", "./build.sh"), "unknown worker")
	f.fails(in.agent("job", "run", "lab-mac", "app", "--", "make"), "a job starts an executable script committed")
	f.fails(in.agent("job", "run", "lab-mac", "app", "--", "./../escape.sh"), "stay inside the repository")
	f.fails(in.agent("job", "run", "lab-mac", "app", "--", "./missing.sh"), "is not an executable file committed")
	f.fails(in.agent("job", "run", "lab-mac", "app", "--", "./notes.sh"), "is not an executable file committed")
	f.fails(in.agent("job", "run", "lab-mac", "app", "--fetch", "../x", "--", "./build.sh"), "stay inside the repository")
	f.fails(in.agent("job", "run", "lab-mac", "app", "--fetch", ".git", "--", "./build.sh"), ".git is not part of a job")
	f.fails(in.agent("job", "run", "lab-mac", "missing", "--", "./build.sh"), "is not a repository of garden")
	f.fails(in.agent("job", "run", "lab-mac", "app", "./build.sh"), "Usage: agent job run")
	f.fails(f.agent("job", "run", "lab-mac", "app", "--", "./build.sh"), "must run in a project session")
	id := f.queueJob(map[string]any{"offline": true}, "--", "./build.sh")
	f.ok(f.agent("job", "cancel", id))
	f.fails(f.agent("job", "cancel", id), "only a queued job can be cancelled")
	f.setWorker(map[string]any{})
	f.ok(f.agent("job", "--drain"))
	if job := f.job(id); job["status"] != "cancelled" {
		t.Fatalf("a cancelled job ran %v", job)
	}
}

func TestWorkerEndpointAcceptsOnlyItsOperations(t *testing.T) {
	if implementation.kind == "python" {
		t.Skip("jobs exist only in the Go agent")
	}
	f := newFixture(t, "laptop")
	for _, operation := range []string{"", "bash", "ping; id", "work-cleanup", "work-cleanup --really", "work-cleanup --really abc",
		"work-cleanup --really " + strings.Repeat("a", 64) + " x", "agent ls"} {
		f.fails(f.with("SSH_ORIGINAL_COMMAND="+operation).internal("--worker-endpoint"), "worker endpoint accepts only")
	}
	if out := f.ok(f.with("SSH_ORIGINAL_COMMAND=ping").internal("--worker-endpoint")).stdout; out != "ok\n" {
		t.Fatalf("ping %q", out)
	}
	commit := strings.Repeat("a", 40)
	for spec, message := range map[string]string{
		`{"id":"20261122T100000-abcdef","project":"garden","repo":"app","commit":"` + commit + `","args":["bash","-c","id"]}`:                "a job starts an executable script",
		`{"id":"20261122T100000-abcdef","project":"garden","repo":"..","commit":"` + commit + `","args":["./b.sh"]}`:                         "invalid repository name",
		`{"id":"20261122T100000-abcdef","project":"garden","repo":"app","commit":"` + commit + `","args":["./.git/hooks/x"]}`:                ".git is not part of a job",
		`{"id":"20261122T100000-abcdef","project":"garden","repo":"app","commit":"` + commit + `","args":["./b.sh"],"fetch":["../../.ssh"]}`: "stay inside the repository",
	} {
		f.fails(f.with("SSH_ORIGINAL_COMMAND=job").input(spec+"\n").internal("--worker-endpoint"), message)
	}
	fetch := `{"id":"20261122T100000-abcdef","project":"garden","repo":"app","path":"../../.ssh"}` + "\n"
	f.fails(f.with("SSH_ORIGINAL_COMMAND=fetch").input(fetch).internal("--worker-endpoint"), "stay inside the repository")
	if f.exists("Builds/garden") {
		t.Fatal("a refused job touched ~/Builds")
	}
	// A linked project folder is refused before anything is written through it.
	must(t, os.MkdirAll(f.path("Builds"), 0o700))
	must(t, os.MkdirAll(f.path("elsewhere"), 0o700))
	must(t, os.Symlink(f.path("elsewhere"), f.path("Builds/garden")))
	good := `{"id":"20261122T100000-abcdef","project":"garden","repo":"app","commit":"` + commit + `","args":["./b.sh"]}` + "\n"
	f.fails(f.with("SSH_ORIGINAL_COMMAND=job").input(good).internal("--worker-endpoint"), "must be a folder, not a link")
	if entries, _ := os.ReadDir(f.path("elsewhere")); len(entries) != 0 {
		t.Fatalf("wrote through a link: %v", entries)
	}
}

func TestWorkerEndpointReplaysAFinishedJob(t *testing.T) {
	f, workerHome := jobFixture(t, countingScript)
	id := f.queueJob(map[string]any{}, "--", "./build.sh", "once")
	f.ok(f.agent("job", "--drain"))
	// The same job again, as after a connection lost just before the exit code arrived.
	spec := `{"id":"` + id + `","project":"garden","repo":"app","commit":"` + f.job(id)["commit"].(string) + `","args":["./build.sh","once"]}` + "\n"
	worker := &fixture{t: t, dir: f.dir, home: workerHome, host: "lab-mac", registry: f.registry}
	replay := worker.with("SSH_ORIGINAL_COMMAND=job").input(spec).internal("--worker-endpoint")
	if replay.code != 0 || !strings.Contains(replay.stdout, "log line once") {
		t.Fatalf("replay\n%s", replay)
	}
	if runs, _ := os.ReadFile(filepath.Join(workerHome, "runs")); string(runs) != "run\n" {
		t.Fatalf("the build ran %q, want once", runs)
	}
}

func TestJobOnAnOnlineWorkerRunsWithoutATimer(t *testing.T) {
	f, _ := jobFixture(t, "#!/bin/sh\necho quick\n")
	out := f.ok(f.with("PI_PROJECT=garden").agent("job", "run", "lab-mac", "app", "--", "./build.sh"))
	if !strings.Contains(out.stdout, "which is online and runs it now") {
		t.Fatalf("queue message\n%s", out)
	}
	id := jobIDPattern.FindStringSubmatch(out.stdout)[1]
	deadline := time.Now().Add(20 * time.Second)
	for f.job(id)["status"] != "done" {
		if time.Now().After(deadline) {
			t.Fatalf("the background drain did not finish the job %v", f.job(id))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(f.gardenInbox()) != 1 {
		t.Fatalf("inbox %v", f.gardenInbox())
	}
}

func TestJobFetchOfTheWholeCheckoutLeavesGitBehind(t *testing.T) {
	f, _ := jobFixture(t, "#!/bin/sh\n")
	f.fails(f.with("PI_PROJECT=garden").agent("job", "run", "lab-mac", "app", "--fetch", "x/.GIT/config", "--", "./build.sh"), ".git is not part of a job")
	id := f.queueJob(map[string]any{}, "--fetch", ".", "--", "./build.sh")
	f.ok(f.agent("job", "--drain"))
	if job := f.job(id); job["status"] != "done" {
		t.Fatalf("job %v", job)
	}
	artifacts := filepath.Join(f.home, ".local/state/agents/jobs", id, "artifacts")
	if _, err := os.Stat(filepath.Join(artifacts, "build.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(artifacts, ".git")); !os.IsNotExist(err) {
		t.Fatalf("git metadata was fetched: %v", err)
	}
}

func TestJobRefusalTellsEachSessionOnce(t *testing.T) {
	f, _ := jobFixture(t, "#!/bin/sh\n")
	f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	refuse := map[string]any{"refuse": "Permission denied (publickey)."}
	first := f.queueJob(refuse, "--", "./build.sh")
	second := f.queueJob(refuse, "--", "./build.sh", "two")
	f.setWorker(map[string]any{"offline": true})
	out := f.ok(f.with("PI_PROJECT=garden", "PI_TASK=audit").agent("job", "run", "lab-mac", "app", "--", "./build.sh"))
	third := jobIDPattern.FindStringSubmatch(out.stdout)[1]
	f.setWorker(refuse)
	f.ok(f.agent("job", "--drain"))
	f.ok(f.agent("job", "--drain"))
	if items := f.gardenInbox(); len(items) != 1 || items[0]["inReplyTo"] != first && items[0]["inReplyTo"] != second {
		t.Fatalf("want one project notice for two jobs, got %v", items)
	}
	for _, id := range []string{first, second, third} {
		if job := f.job(id); job["refusal"] != "Permission denied (publickey)." || job["status"] != "queued" {
			t.Fatalf("job %s %v", id, job)
		}
	}
	messages := filepath.Join(f.workspace("garden"), ".agent/tasks/audit/messages")
	entries, err := os.ReadDir(messages)
	must(t, err)
	if len(entries) != 1 || !strings.Contains(f.read(filepath.Join(messages, entries[0].Name())), "refuses job "+third) {
		t.Fatalf("task notices %v", entries)
	}
}

func TestJobResultReachesItsTaskOnceAndAClosedTasksProject(t *testing.T) {
	f, _ := jobFixture(t, "#!/bin/sh\necho built\n")
	f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	f.setWorker(map[string]any{"offline": true})
	queue := func() string {
		out := f.ok(f.with("PI_PROJECT=garden", "PI_TASK=audit").agent("job", "run", "lab-mac", "app", "--", "./build.sh"))
		return jobIDPattern.FindStringSubmatch(out.stdout)[1]
	}
	id := queue()
	f.setWorker(map[string]any{})
	f.ok(f.agent("job", "--drain"))
	// Queued for the task but not taken yet: delivery stays pending.
	job := f.job(id)
	if job["status"] != "delivering" || job["outcome"] != "done" {
		t.Fatalf("job before the task takes it %v", job)
	}
	messages := filepath.Join(f.workspace("garden"), ".agent/tasks/audit/messages")
	name := job["delivery"].(string) + ".md"
	if !strings.Contains(f.read(filepath.Join(messages, name)), "finished, exit 0") {
		t.Fatalf("no result for the task in %s", messages)
	}
	f.ok(f.agent("job", "--drain"))
	if entries, _ := os.ReadDir(messages); len(entries) != 1 {
		t.Fatalf("a pending result was queued twice %v", entries)
	}
	// The task takes it, as its extension does. The next drain sees it and makes no second copy.
	must(t, os.MkdirAll(filepath.Join(messages, "delivered"), 0o700))
	must(t, os.Rename(filepath.Join(messages, name), filepath.Join(messages, "delivered", name)))
	f.ok(f.agent("job", "--drain"))
	if job := f.job(id); job["status"] != "done" {
		t.Fatalf("job after the task took it %v", job)
	}
	entries, err := os.ReadDir(messages)
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != "delivered" {
		t.Fatalf("task messages after delivery %v", entries)
	}
	if len(f.gardenInbox()) != 0 {
		t.Fatal("a task's result went to the project too")
	}
	// A task that starts closing before it takes a result hands it to the project.
	f.setWorker(map[string]any{"offline": true})
	closing := queue()
	f.setWorker(map[string]any{})
	f.ok(f.agent("job", "--drain"))
	metaPath := filepath.Join(f.workspace("garden"), ".agent/tasks/audit/meta.json")
	meta := readJSON[map[string]any](t, metaPath)
	meta["status"] = "closing"
	f.write(metaPath, mustJSON(t, meta))
	f.ok(f.agent("job", "--drain"))
	if job := f.job(closing); job["status"] != "done" {
		t.Fatalf("job of a closing task %v", job)
	}
	if items := f.gardenInbox(); len(items) != 1 || items[0]["inReplyTo"] != closing || !strings.Contains(items[0]["text"].(string), "Task audit was closed") {
		t.Fatalf("project inbox %v", items)
	}
	for _, path := range []string{filepath.Join(f.workspace("garden"), ".agent/inbox")} {
		for _, entry := range jsonFiles(t, path) {
			must(t, os.Remove(filepath.Join(path, entry["id"].(string)+".json")))
		}
	}
	f.setWorker(map[string]any{"offline": true})
	late := queue()
	meta = readJSON[map[string]any](t, metaPath)
	meta["status"] = "closed"
	f.write(metaPath, mustJSON(t, meta))
	f.setWorker(map[string]any{})
	f.ok(f.agent("job", "--drain"))
	items := f.gardenInbox()
	if len(items) != 1 || items[0]["inReplyTo"] != late || !strings.Contains(items[0]["text"].(string), "Task audit was closed") {
		t.Fatalf("project inbox %v", items)
	}
}
