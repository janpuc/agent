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
const Usage = "Usage: agent job run <worker> <repo> [--commit <rev>] [--fetch <path>]... -- <command>..."

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
	id, err := requests.NewID()
	if err != nil {
		return err
	}
	job := Job{Spec: Spec{ID: id, Project: project, Repo: repo, Commit: commit, Args: command},
		Worker: worker, Task: os.Getenv("PI_TASK"), Fetch: fetch, Status: "queued", Created: requests.Timestamp()}
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
	if online, _ := ping(worker); online {
		startDrain()
		fmt.Printf("Job %s queued for %s, which is online and runs it now. The result arrives in this session as a reply to %s. Do not wait or poll for it.\n", id, worker, id)
		return nil
	}
	fmt.Printf("%s is offline, which is normal: Jan switches it on when he wants. Job %s is queued and starts by itself once %s is on. "+
		"The result arrives in this session as a reply to %s. Tell Jan once that the job waits for %s, then go on with other work or stop. "+
		"Do not retry, poll, or build on another machine.\n", worker, id, worker, id, worker)
	return nil
}

func gitOutput(dir string, args ...string) (string, error) {
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(output)), err
}

// ping reports whether the worker answers its endpoint, with the last SSH error line otherwise.
func ping(worker string) (bool, string) {
	ctx, stop := context.WithTimeout(context.Background(), pingTimeout)
	defer stop()
	command := exec.CommandContext(ctx, "ssh", worker, "ping")
	command.Stdin = nil
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		return false, lines[len(lines)-1]
	}
	return strings.TrimSpace(stdout.String()) == "ok", ""
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
		owner := job.Project
		if job.Task != "" {
			owner += "/" + job.Task
		}
		exit := ""
		if job.Exit != nil {
			exit = fmt.Sprintf(" exit=%d", *job.Exit)
		}
		fmt.Printf("%s %s status=%s%s %s %s@%s %s\n", job.ID, job.Worker, job.Status, exit, owner, job.Repo,
			job.Commit[:min(12, len(job.Commit))], strings.Join(job.Args, " "))
	}
	return nil
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
	path := filepath.Join(Dir(home), id, "job.json")
	job, err := load(path)
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

// Drain runs every queued job whose worker answers, oldest first, and delivers each result. A
// worker that does not answer keeps its jobs queued for the next drain. The timer runs it.
func Drain(reg *registry.Registry, home, host, root string) error {
	unlock, err := lockDrain(home, false)
	if err != nil || unlock == nil {
		return err
	}
	defer unlock()
	offline := map[string]bool{}
	for {
		jobs, err := all(home)
		if err != nil {
			return err
		}
		ran := false
		for _, job := range jobs {
			if job.Status == "running" {
				// A drain that died mid-job left it running. Run it again, it may have been cut off.
				job.Status = "queued"
			}
			if job.Status != "queued" || offline[job.Worker] {
				continue
			}
			if !workerKnown(reg, job.Worker) {
				continue
			}
			if online, _ := ping(job.Worker); !online {
				offline[job.Worker] = true
				continue
			}
			if !runJob(reg, home, host, root, job) {
				offline[job.Worker] = true
			}
			ran = true
		}
		if !ran {
			return nil
		}
	}
}

// runJob runs one job and delivers its result. It returns false when the connection was lost,
// in which case the job is queued again and waits for the worker.
func runJob(reg *registry.Registry, home, host, root string, job Job) bool {
	folder := filepath.Join(Dir(home), job.ID)
	job.Status, job.Started, job.Attempts = "running", requests.Timestamp(), job.Attempts+1
	if err := save(home, job); err != nil {
		return true
	}
	logFile, err := os.OpenFile(filepath.Join(folder, "log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		finish(reg, home, host, root, job, -1, "cannot open the job log: "+err.Error())
		return true
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "--- attempt %d on %s at %s: %s\n", job.Attempts, job.Worker, job.Started, strings.Join(job.Args, " "))
	bundle, err := makeBundle(filepath.Join(root, job.Project, job.Repo), job)
	if err != nil {
		finish(reg, home, host, root, job, -1, "cannot bundle the commit: "+err.Error())
		return true
	}
	defer os.Remove(bundle)
	code, err := remote(job.Worker, "job", job.Spec, bundle, logFile, logFile, runLimit+15*time.Minute)
	if err != nil {
		finish(reg, home, host, root, job, -1, "cannot start ssh: "+err.Error())
		return true
	}
	if code == 255 {
		fmt.Fprintf(logFile, "--- the connection to %s was lost, the job waits for it\n", job.Worker)
		if job.Attempts < maxAttempts {
			job.Status, job.Note = "queued", fmt.Sprintf("connection to %s lost on attempt %d", job.Worker, job.Attempts)
			save(home, job)
			return false
		}
		finish(reg, home, host, root, job, code, fmt.Sprintf("the connection to %s was lost %d times", job.Worker, job.Attempts))
		return false
	}
	note := ""
	if code == 0 {
		note = fetchAll(job, folder, logFile)
	}
	finish(reg, home, host, root, job, code, note)
	return true
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

// fetchAll copies the job's --fetch paths into artifacts/ and returns a note on any failure.
func fetchAll(job Job, folder string, log io.Writer) string {
	var failed []string
	for _, path := range job.Fetch {
		spec := Spec{ID: job.ID, Project: job.Project, Repo: job.Repo, Path: path}
		reader, writer := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- untar(reader, filepath.Join(folder, "artifacts")) }()
		code, err := remote(job.Worker, "fetch", spec, "", writer, log, time.Hour)
		writer.Close()
		extractErr := <-done
		if err != nil || code != 0 || extractErr != nil {
			failed = append(failed, path)
			fmt.Fprintf(log, "--- fetch %s failed: exit %d %v %v\n", path, code, err, extractErr)
		}
	}
	if len(failed) > 0 {
		return "could not fetch " + strings.Join(failed, ", ")
	}
	return ""
}

// untar extracts regular files and folders below dest. Any other entry or a path that leaves
// dest fails the extraction.
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
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o700|0o600)
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

// finish records the outcome and hands it to the session that queued the job.
func finish(reg *registry.Registry, home, host, root string, job Job, code int, note string) {
	job.Exit, job.Finished, job.Note = &code, requests.Timestamp(), note
	job.Status = "done"
	if code != 0 || note != "" {
		job.Status = "failed"
	}
	save(home, job)
	if err := deliver(reg, home, host, root, job); err != nil {
		job.Note = strings.TrimSpace(job.Note + " result not delivered: " + err.Error())
		save(home, job)
	}
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

// deliver puts the result in the project inbox as a reply to the job id, or sends it to the task
// that queued the job, which starts the task if it stopped.
func deliver(reg *registry.Registry, home, host, root string, job Job) error {
	text := message(home, job)
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
	id, err := requests.NewID()
	if err != nil {
		return err
	}
	item := requests.Request{ID: id, From: job.Worker, To: job.Project, Text: text, Created: requests.Timestamp(),
		Status: "new", InReplyTo: job.ID}
	return requests.Store(reg, home, host, root, job.Project, id, item)
}
