// Package jobs runs work on worker hosts: machines such as a laptop that are switched on only when
// their owner wants. An execution host queues a job, a drain hands it to the worker over SSH once
// the worker answers, and the result reaches the session that queued it like a request reply.
// A worker that is off is normal. The job waits for it, nobody retries or looks for another machine.
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/janpuc/agent/internal/registry"
)

const (
	maxArgs     = 64
	maxArg      = 4096
	maxSpec     = 64 << 10
	maxBundle   = 4 << 30
	maxAttempts = 6
	tailLines   = 40
	tailBytes   = 6 << 10
)

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	idPattern     = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}-[0-9a-f]{6}$`)
)

// Spec is what the worker receives: one JSON line on stdin, followed by a git bundle for a job.
type Spec struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Repo    string `json:"repo"`
	Commit  string `json:"commit,omitempty"`
	// Args start with an executable script committed in the repository, as ./path, so what runs
	// on the worker is reviewable in git. The worker checks it in the commit before running it.
	Args []string `json:"args,omitempty"`
	// Fetch lists the files or folders a job keeps after it succeeds, relative to the checkout.
	Fetch []string `json:"fetch,omitempty"`
	// Path is the kept file or folder a fetch returns.
	Path string `json:"path,omitempty"`
}

// Job is a queued or finished job on the execution host.
type Job struct {
	Spec
	Worker string `json:"worker"`
	Task   string `json:"task,omitempty"`
	// Status is queued, running, fetching, delivering, done, failed or cancelled. fetching and
	// delivering follow a finished build, so a retry never runs the build again.
	Status   string `json:"status"`
	Created  string `json:"created"`
	Started  string `json:"started,omitempty"`
	Finished string `json:"finished,omitempty"`
	Attempts int    `json:"attempts"`
	Exit     *int   `json:"exit,omitempty"`
	Note     string `json:"note,omitempty"`
	// Fetches counts attempts to fetch the kept files of a finished build.
	Fetches int `json:"fetches,omitempty"`
	// Refusal is the last reason the worker gave for refusing jobs, reported to the project once.
	Refusal string `json:"refusal,omitempty"`
	// Outcome is done or failed once the result is recorded, Status follows once it is delivered.
	Outcome string `json:"outcome,omitempty"`
	// Delivery is the inbox item id of the result, so a repeated delivery overwrites it.
	Delivery    string `json:"delivery,omitempty"`
	Undelivered string `json:"undelivered,omitempty"`
}

// Dir holds one folder per job: job.json, log and artifacts/.
func Dir(home string) string { return filepath.Join(registry.StateDir(home), "jobs") }

func validName(kind, name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid %s name: %q", kind, name)
	}
	return nil
}

// validPath accepts a relative path that stays inside the checkout and out of its .git.
func validPath(path string) error {
	if path == "" || strings.ContainsRune(path, 0) || !filepath.IsLocal(path) {
		return fmt.Errorf("invalid path %q: it must be relative and stay inside the repository", path)
	}
	if slices.ContainsFunc(strings.Split(filepath.ToSlash(filepath.Clean(path)), "/"),
		func(part string) bool { return strings.EqualFold(part, ".git") }) {
		return fmt.Errorf("invalid path %q: .git is not part of a job", path)
	}
	return nil
}

// validArgs checks a job command on both hosts.
func validArgs(args []string) error {
	if len(args) == 0 {
		return errors.New("a job needs a command after --")
	}
	if len(args) > maxArgs {
		return fmt.Errorf("a job command has at most %d arguments", maxArgs)
	}
	for _, arg := range args {
		if len(arg) > maxArg || strings.ContainsRune(arg, 0) {
			return errors.New("a job argument is too long or contains NUL")
		}
	}
	script, ok := strings.CutPrefix(args[0], "./")
	if !ok {
		return fmt.Errorf("a job starts an executable script committed in the repository, as ./path, not %q", args[0])
	}
	return validPath(script)
}

func (s Spec) validate(job bool) error {
	if !idPattern.MatchString(s.ID) {
		return fmt.Errorf("invalid job id %q", s.ID)
	}
	if err := validName("project", s.Project); err != nil {
		return err
	}
	if err := validName("repository", s.Repo); err != nil {
		return err
	}
	if job {
		if !commitPattern.MatchString(s.Commit) {
			return fmt.Errorf("invalid commit %q", s.Commit)
		}
		if len(s.Fetch) > maxArgs {
			return fmt.Errorf("a job keeps at most %d paths", maxArgs)
		}
		for _, path := range s.Fetch {
			if err := validPath(path); err != nil {
				return err
			}
		}
		return validArgs(s.Args)
	}
	return validPath(s.Path)
}

func load(path string) (Job, error) {
	var job Job
	data, err := os.ReadFile(path)
	if err != nil {
		return job, err
	}
	if err := json.Unmarshal(data, &job); err != nil {
		return job, fmt.Errorf("invalid job %s: %w", path, err)
	}
	return job, nil
}

func save(home string, job Job) error {
	folder := filepath.Join(Dir(home), job.ID)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(folder, "job.json")
	temp := path + ".tmp"
	defer os.Remove(temp)
	if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// all returns every job, oldest first. Unreadable records are skipped, never deleted.
func all(home string) ([]Job, error) {
	entries, err := os.ReadDir(Dir(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for _, entry := range entries {
		if !entry.IsDir() || !idPattern.MatchString(entry.Name()) {
			continue
		}
		if job, err := load(filepath.Join(Dir(home), entry.Name(), "job.json")); err == nil {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}
