package jobs

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/janpuc/agent/internal/actions"
	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/requests"
)

const (
	pingTimeout = 20 * time.Second
	// A job runs at most runLimit on the worker, which enforces it. The drain waits a little longer.
	runLimit = 3 * time.Hour
)

// Usage is the job command's Usage line.
const Usage = "Usage: agent job run <worker> <repo> [--commit <rev>] [--fetch <path>]... -- ./<script> [args]..."

// Entry runs agent job run, ls, cancel and --drain on an execution host.
func Entry(reg *registry.Registry, home, host, root string, args []string) error {
	if len(args) == 0 {
		return errors.New(Usage)
	}
	switch args[0] {
	case "run":
		return queue(reg, home, host, root, args[1:])
	case "ls":
		if len(args) != 1 {
			return errors.New("Usage: agent job ls")
		}
		return list(home)
	case "cancel":
		if len(args) != 2 {
			return errors.New("Usage: agent job cancel <id>")
		}
		return cancel(home, args[1])
	case "--drain":
		return Drain(reg, home, host, root)
	}
	return errors.New(Usage)
}

func workerKnown(reg *registry.Registry, name string) bool {
	return slices.Contains(reg.Platform.Workers, name)
}

// queue records a job for the project of this session and starts a drain when the worker answers.
func queue(reg *registry.Registry, home, host, root string, args []string) error {
	split := slices.Index(args, "--")
	if split < 2 {
		return errors.New(Usage)
	}
	command, options := args[split+1:], args[:split]
	worker, repo := options[0], options[1]
	rev := "HEAD"
	var fetch []string
	for i := 2; i < len(options); i += 2 {
		if i+1 >= len(options) {
			return errors.New(Usage)
		}
		switch options[i] {
		case "--commit":
			rev = options[i+1]
		case "--fetch":
			if err := validPath(options[i+1]); err != nil {
				return err
			}
			fetch = append(fetch, filepath.Clean(options[i+1]))
		default:
			return errors.New(Usage)
		}
	}
	if !workerKnown(reg, worker) {
		return fmt.Errorf("unknown worker %q, the registry lists %s", worker, strings.Join(reg.Platform.Workers, ", "))
	}
	project := os.Getenv("PI_PROJECT")
	if _, ok := reg.Projects[project]; !ok || reg.ProjectHost(project) != host {
		return errors.New("agent job run must run in a project session on this host")
	}
	if err := validName("repository", repo); err != nil {
		return err
	}
	if err := validArgs(command); err != nil {
		return err
	}
	checkout := filepath.Join(root, project, repo)
	commit, err := gitOutput(checkout, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return fmt.Errorf("%s is not a repository of %s with commit %s", repo, project, rev)
	}
	script := strings.TrimPrefix(command[0], "./")
	if tree, _ := gitOutput(checkout, "ls-tree", commit, "--", script); !strings.HasPrefix(tree, "100755 blob ") {
		return fmt.Errorf("%s is not an executable file committed at %s. Commit the script with mode 755 first", command[0], commit[:12])
	}
	id, err := requests.NewID()
	if err != nil {
		return err
	}
	job := Job{Spec: Spec{ID: id, Project: project, Repo: repo, Commit: commit, Args: command, Fetch: fetch},
		Worker: worker, Task: os.Getenv("PI_TASK"), Status: "queued", Created: requests.Timestamp()}
	if err := job.validate(true); err != nil {
		return err
	}
	if err := save(home, job); err != nil {
		return err
	}
	actions.Append(home, host, "a job", fmt.Sprintf("agent job run %s %s at %s: %s", worker, repo, commit[:12], strings.Join(command, " ")))
	if dirty, _ := gitOutput(checkout, "status", "--porcelain", "--untracked-files=no"); dirty != "" {
		fmt.Printf("Note: %s has uncommitted changes. The job builds commit %s only.\n", repo, commit[:12])
	}
	reply := fmt.Sprintf("The result arrives in this session as a reply to %s.", id)
	switch state, reason := ping(worker); state {
	case online:
		startDrain()
		fmt.Printf("Job %s queued for %s, which is online and runs it now. %s Do not wait or poll for it.\n", id, worker, reply)
	case offline:
		fmt.Printf("%s is offline, which is normal: Jan switches it on when he wants. Job %s is queued and starts by itself once %s is on. "+
			"%s Tell Jan once that the job waits for %s, then go on with other work or stop. "+
			"Do not retry, poll, or build on another machine.\n", worker, id, worker, reply, worker)
	default:
		fmt.Printf("%s answers but refuses jobs: %s. This needs Jan or dotfiles, not a retry. Job %s is queued and starts by itself once it is fixed. "+
			"%s Tell Jan once, quoting that reason, then go on with other work or stop. "+
			"Do not retry, poll, or build on another machine.\n", worker, reason, id, reply)
	}
	return nil
}

func gitOutput(dir string, args ...string) (string, error) {
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(output)), err
}

type reachability int

const (
	online reachability = iota
	offline
	refused
)

// Signs of a machine that is off, asleep or out of the network. Anything else that fails is a
// setup problem, such as an unpinned or changed host key, a rejected key or a wrong endpoint.
var offlineSigns = []string{"timed out", "Connection refused", "No route to host", "Network is unreachable",
	"Host is down", "Could not resolve hostname", "Connection closed by remote host", "Connection reset"}

// ping reports whether the worker answers its endpoint, and why it refuses when it does not.
func ping(worker string) (reachability, string) {
	ctx, stop := context.WithTimeout(context.Background(), pingTimeout)
	defer stop()
	command := exec.CommandContext(ctx, "ssh", worker, "ping")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err == nil && strings.TrimSpace(stdout.String()) == "ok" {
		return online, ""
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	last := lines[len(lines)-1]
	var exit *exec.ExitError
	if ctx.Err() != nil || errors.As(err, &exit) && exit.ExitCode() == 255 && (last == "" || slices.ContainsFunc(offlineSigns,
		func(sign string) bool { return strings.Contains(last, sign) })) {
		return offline, ""
	}
	if last == "" {
		last = "the endpoint did not answer ok to ping"
	}
	return refused, last
}

// startDrain runs a drain in its own session, so it outlives the command that queued the job.
func startDrain() {
	self, err := os.Executable()
	if err != nil {
		return
	}
	command := exec.Command(self, "job", "--drain")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if command.Start() == nil {
		command.Process.Release()
	}
}

func list(home string) error {
	jobs, err := all(home)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Println("No jobs")
	}
	for _, job := range jobs {
		exit := ""
		if job.Exit != nil {
			exit = fmt.Sprintf(" exit=%d", *job.Exit)
		}
		note := ""
		if job.Note != "" {
			note = " (" + job.Note + ")"
		}
		fmt.Printf("%s %s status=%s%s %s %s@%s %s%s\n", job.ID, job.Worker, job.Status, exit, owner(job), job.Repo,
			job.Commit[:min(12, len(job.Commit))], strings.Join(job.Args, " "), note)
	}
	return nil
}

func owner(job Job) string {
	if job.Task != "" {
		return job.Project + "/" + job.Task
	}
	return job.Project
}

// Waiting lists jobs that wait for a worker, one line each, for agent todo.
func Waiting(home string) []string {
	jobs, _ := all(home)
	var lines []string
	for _, job := range jobs {
		if job.Status != "queued" && job.Status != "running" && job.Status != "fetching" {
			continue
		}
		line := fmt.Sprintf("job %s for %s waits for %s to be switched on", job.ID, owner(job), job.Worker)
		if job.Refusal != "" {
			line = fmt.Sprintf("job %s for %s waits: %s refuses jobs: %s", job.ID, owner(job), job.Worker, job.Refusal)
		}
		lines = append(lines, line)
	}
	return lines
}

func cancel(home, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid job id %q", id)
	}
	unlock, err := lockDrain(home, true)
	if err != nil {
		return err
	}
	defer unlock()
	job, err := load(filepath.Join(Dir(home), id, "job.json"))
	if err != nil {
		return fmt.Errorf("no job %s", id)
	}
	if job.Status != "queued" {
		return fmt.Errorf("job %s is %s, only a queued job can be cancelled", id, job.Status)
	}
	job.Status, job.Finished = "cancelled", requests.Timestamp()
	if err := save(home, job); err != nil {
		return err
	}
	fmt.Printf("Job %s cancelled\n", id)
	return nil
}

// lockDrain takes the drain lock. A drain that finds it held leaves the work to the holder.
// cancel waits for it instead, so it never races a drain starting the same job.
func lockDrain(home string, wait bool) (func(), error) {
	if err := os.MkdirAll(Dir(home), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(Dir(home), ".drain.lock"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	return func() { file.Close() }, nil
}

// Drain moves every unfinished job forward, oldest first, while its worker answers. A worker
// that does not answer keeps its jobs where they are for the next drain. The timer runs it.
// A record it cannot write stops the drain, so nothing runs twice on a lost record.
func Drain(reg *registry.Registry, home, host, root string) error {
	unlock, err := lockDrain(home, false)
	if err != nil || unlock == nil {
		return err
	}
	defer unlock()
	waiting := map[string]bool{}
	for {
		jobs, err := all(home)
		if err != nil {
			return err
		}
		moved := false
		for _, job := range jobs {
			if job.Status == "delivering" {
				// The result was recorded but not handed over. Deliver it, the worker is not needed.
				if err := deliver(reg, home, host, root, job); err != nil {
					return err
				}
				continue
			}
			if job.Status != "queued" && job.Status != "running" && job.Status != "fetching" || waiting[job.Worker] {
				continue
			}
			if !workerKnown(reg, job.Worker) {
				if err := complete(reg, home, host, root, job, -1, job.Worker+" is no longer a worker in the registry"); err != nil {
					return err
				}
				continue
			}
			state, reason := ping(job.Worker)
			if state != online {
				waiting[job.Worker] = true
				if err := noteRefusal(reg, home, host, root, job, state, reason); err != nil {
					return err
				}
				continue
			}
			if job.Refusal != "" {
				job.Refusal = ""
				if err := save(home, job); err != nil {
					return err
				}
			}
			advanced, err := step(reg, home, host, root, job)
			if err != nil {
				return err
			}
			if !advanced {
				waiting[job.Worker] = true
			}
			moved = true
		}
		if !moved {
			return nil
		}
	}
}

// noteRefusal tells the project once per reason that a worker answers but refuses jobs, so the
// agent can tell Jan. A worker that is merely off is not news.
func noteRefusal(reg *registry.Registry, home, host, root string, job Job, state reachability, reason string) error {
	if state != refused || job.Refusal == reason {
		return nil
	}
	job.Refusal = reason
	if err := save(home, job); err != nil {
		return err
	}
	text := fmt.Sprintf("%s answers but refuses job %s: %s. This needs Jan or dotfiles, not a retry. The job stays queued and "+
		"starts by itself once it is fixed. Tell Jan once, quoting the reason, and do not retry or build elsewhere.\n", job.Worker, job.ID, reason)
	return send(reg, home, host, root, job, text, "")
}

// step runs the build or fetches its files, whichever the job needs next. It returns false when
// the connection was lost, which leaves the job for the next drain.
func step(reg *registry.Registry, home, host, root string, job Job) (bool, error) {
	folder := filepath.Join(Dir(home), job.ID)
	logFile, err := os.OpenFile(filepath.Join(folder, "log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer logFile.Close()
	if job.Status == "fetching" {
		return fetchStep(reg, home, host, root, job, folder, logFile)
	}
	job.Status, job.Started, job.Attempts = "running", requests.Timestamp(), job.Attempts+1
	if err := save(home, job); err != nil {
		return false, err
	}
	fmt.Fprintf(logFile, "--- attempt %d on %s at %s: %s\n", job.Attempts, job.Worker, job.Started, strings.Join(job.Args, " "))
	bundle, err := makeBundle(filepath.Join(root, job.Project, job.Repo), job)
	if err != nil {
		return true, complete(reg, home, host, root, job, -1, "cannot bundle the commit: "+err.Error())
	}
	defer os.Remove(bundle)
	// The worker replays a finished run and follows one still running, so this never builds twice.
	code, err := remote(job.Worker, "job", job.Spec, bundle, logFile, logFile, runLimit+15*time.Minute)
	if err != nil {
		return true, complete(reg, home, host, root, job, -1, "cannot start ssh: "+err.Error())
	}
	if code == 255 {
		fmt.Fprintf(logFile, "--- the connection to %s was lost, the job waits for it\n", job.Worker)
		if job.Attempts >= maxAttempts {
			return false, complete(reg, home, host, root, job, code, fmt.Sprintf("the connection to %s was lost %d times", job.Worker, job.Attempts))
		}
		job.Status, job.Note = "queued", fmt.Sprintf("connection to %s lost on attempt %d", job.Worker, job.Attempts)
		return false, save(home, job)
	}
	job.Exit, job.Note = &code, ""
	if code != 0 || len(job.Fetch) == 0 {
		return true, complete(reg, home, host, root, job, code, "")
	}
	job.Status = "fetching"
	if err := save(home, job); err != nil {
		return false, err
	}
	return fetchStep(reg, home, host, root, job, folder, logFile)
}

// fetchStep copies the job's kept paths into artifacts/. A lost connection leaves the job
// fetching for the next drain, the build is not run again.
func fetchStep(reg *registry.Registry, home, host, root string, job Job, folder string, log io.Writer) (bool, error) {
	artifacts := filepath.Join(folder, "artifacts")
	if err := os.RemoveAll(artifacts); err != nil {
		return false, err
	}
	job.Fetches++
	var failed []string
	for _, path := range job.Fetch {
		spec := Spec{ID: job.ID, Project: job.Project, Repo: job.Repo, Path: path}
		reader, writer := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- untar(reader, artifacts) }()
		code, err := remote(job.Worker, "fetch", spec, "", writer, log, time.Hour)
		writer.Close()
		extractErr := <-done
		if code == 255 && err == nil {
			fmt.Fprintf(log, "--- the connection to %s was lost while fetching %s\n", job.Worker, path)
			if job.Fetches >= maxAttempts {
				return false, complete(reg, home, host, root, job, *job.Exit, fmt.Sprintf("the build passed but its files could not be fetched in %d tries", job.Fetches))
			}
			job.Note = "connection lost while fetching"
			return false, save(home, job)
		}
		if err != nil || code != 0 || extractErr != nil {
			failed = append(failed, path)
			fmt.Fprintf(log, "--- fetch %s failed: exit %d %v %v\n", path, code, err, extractErr)
		}
	}
	note := ""
	if len(failed) > 0 {
		note = "the build passed but these were not kept or fetched: " + strings.Join(failed, ", ")
	}
	return true, complete(reg, home, host, root, job, *job.Exit, note)
}

// makeBundle writes a git bundle of the job's commit. git bundles name refs, so a private ref
// points at the commit while the bundle is written and is removed right after.
func makeBundle(checkout string, job Job) (string, error) {
	ref := "refs/agent-job/" + job.ID
	if _, err := gitOutput(checkout, "update-ref", ref, job.Commit); err != nil {
		return "", err
	}
	defer gitOutput(checkout, "update-ref", "-d", ref)
	file, err := os.CreateTemp("", "agent-job-*.bundle")
	if err != nil {
		return "", err
	}
	file.Close()
	if output, err := exec.Command("git", "-C", checkout, "bundle", "create", "-q", file.Name(), ref).CombinedOutput(); err != nil {
		os.Remove(file.Name())
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(output)))
	}
	return file.Name(), nil
}

// remote runs one worker operation with the spec line and an optional file on stdin.
func remote(worker, operation string, spec Spec, attach string, stdout, stderr io.Writer, limit time.Duration) (int, error) {
	line, err := json.Marshal(spec)
	if err != nil {
		return 0, err
	}
	input := []io.Reader{bytes.NewReader(append(line, '\n'))}
	if attach != "" {
		file, err := os.Open(attach)
		if err != nil {
			return 0, err
		}
		defer file.Close()
		input = append(input, file)
	}
	ctx, stop := context.WithTimeout(context.Background(), limit)
	defer stop()
	command := exec.CommandContext(ctx, "ssh", worker, operation)
	command.Stdin = io.MultiReader(input...)
	command.Stdout, command.Stderr = stdout, stderr
	err = command.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ctx.Err() != nil {
			return 255, nil
		}
		return exit.ExitCode(), nil
	}
	return 0, err
}

// untar extracts regular files and folders below dest. Any other entry, a path that leaves dest
// or a name extracted twice fails the extraction.
func untar(source io.Reader, dest string) error {
	reader := tar.NewReader(source)
	var failure error
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return failure
		}
		if err != nil {
			io.Copy(io.Discard, source)
			return err
		}
		if failure != nil {
			continue
		}
		if !filepath.IsLocal(header.Name) {
			failure = fmt.Errorf("unsafe artifact path %q", header.Name)
			continue
		}
		target := filepath.Join(dest, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			failure = os.MkdirAll(target, 0o700)
		case tar.TypeReg:
			if failure = os.MkdirAll(filepath.Dir(target), 0o700); failure != nil {
				continue
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0o700|0o600)
			if err != nil {
				failure = err
				continue
			}
			_, failure = io.Copy(file, reader)
			file.Close()
		default:
			failure = fmt.Errorf("artifact %q is not a file or folder", header.Name)
		}
	}
}

// complete records the outcome, then delivers it. The record says delivering until the result
// is handed over, so a drain that dies in between delivers it next time.
func complete(reg *registry.Registry, home, host, root string, job Job, code int, note string) error {
	job.Exit, job.Finished, job.Note, job.Refusal = &code, requests.Timestamp(), note, ""
	job.Outcome = "done"
	if code != 0 || note != "" {
		job.Outcome = "failed"
	}
	job.Status = "delivering"
	if job.Delivery == "" {
		id, err := requests.NewID()
		if err != nil {
			return err
		}
		job.Delivery = id
	}
	if err := save(home, job); err != nil {
		return err
	}
	return deliver(reg, home, host, root, job)
}

func message(home string, job Job) string {
	folder := filepath.Join(Dir(home), job.ID)
	var text strings.Builder
	outcome := fmt.Sprintf("exit %d", *job.Exit)
	if *job.Exit < 0 {
		outcome = "did not run"
	}
	fmt.Fprintf(&text, "%s job %s finished, %s: %s in %s at %s (attempt %d).\n", job.Worker, job.ID, outcome,
		strings.Join(job.Args, " "), job.Repo, job.Commit[:12], job.Attempts)
	if job.Note != "" {
		fmt.Fprintf(&text, "Note: %s.\n", job.Note)
	}
	fmt.Fprintf(&text, "Log: %s\n", filepath.Join(folder, "log"))
	if len(job.Fetch) > 0 {
		fmt.Fprintf(&text, "Artifacts: %s\n", filepath.Join(folder, "artifacts"))
	}
	if tail := logTail(filepath.Join(folder, "log")); tail != "" {
		fmt.Fprintf(&text, "\nEnd of the log:\n```\n%s\n```\n", tail)
	}
	return text.String()
}

func logTail(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.Size() > 64<<10 {
		file.Seek(info.Size()-64<<10, io.SeekStart)
	}
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 64<<10)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > tailLines {
			lines = lines[1:]
		}
	}
	tail := strings.Join(lines, "\n")
	if len(tail) > tailBytes {
		tail = tail[len(tail)-tailBytes:]
	}
	return tail
}

// deliver hands the result over and marks the job done or failed. A task that cannot take it,
// because it was closed, leaves it to the project inbox. A result that cannot be delivered at
// all stays delivering for the next drain, which only fails on a record it cannot write.
func deliver(reg *registry.Registry, home, host, root string, job Job) error {
	text := message(home, job)
	err := send(reg, home, host, root, job, text, job.Delivery)
	if err != nil && job.Task != "" {
		project := job
		project.Task = ""
		err = send(reg, home, host, root, project, text+fmt.Sprintf("\nTask %s could not take this result: %v\n", job.Task, err), job.Delivery)
	}
	if err != nil {
		job.Undelivered = err.Error()
		return save(home, job)
	}
	job.Status, job.Undelivered = job.Outcome, ""
	return save(home, job)
}

// send puts text in the project inbox as a reply to the job id, under id when given so a repeat
// overwrites the same item, or sends it to the task that queued the job, which starts the task if
// it stopped.
func send(reg *registry.Registry, home, host, root string, job Job, text, id string) error {
	if job.Task != "" {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		output, err := exec.Command(self, "task", "send", job.Project, job.Task, text).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	if id == "" {
		var err error
		if id, err = requests.NewID(); err != nil {
			return err
		}
	}
	item := requests.Request{ID: id, From: job.Worker, To: job.Project, Text: text, Created: requests.Timestamp(),
		Status: "new", InReplyTo: job.ID}
	return requests.Store(reg, home, host, root, job.Project, id, item)
}
