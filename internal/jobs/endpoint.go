package jobs

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// EndpointUsage lists what the worker endpoint accepts.
const EndpointUsage = "worker endpoint accepts only ping, job, fetch, work-cleanup --dry-run or work-cleanup --really"

// Endpoint is a worker's forced SSH command for the execution host's key. authorized_keys is the
// gate. It runs jobs in ~/Builds/<project>/<repo>, returns files from there, and runs the migrated
// Work cleanup, nothing else.
func Endpoint(home string, stdin io.Reader, stdout, stderr io.Writer) error {
	operation := os.Getenv("SSH_ORIGINAL_COMMAND")
	switch operation {
	case "ping":
		fmt.Fprintln(stdout, "ok")
		return nil
	case "work-cleanup --dry-run", "work-cleanup --really":
		return workCleanup(home, strings.TrimPrefix(operation, "work-cleanup "), stdout, stderr)
	case "job", "fetch":
	default:
		return errors.New(EndpointUsage)
	}
	input := bufio.NewReaderSize(stdin, 64<<10)
	line, err := input.ReadSlice('\n')
	if err != nil || len(line) > maxSpec {
		return errors.New("a worker operation starts with one JSON line of at most 64 KiB")
	}
	var spec Spec
	if err := json.Unmarshal(line, &spec); err != nil {
		return fmt.Errorf("invalid job spec: %w", err)
	}
	if err := spec.validate(operation == "job"); err != nil {
		return err
	}
	builds, err := buildsDir(home)
	if err != nil {
		return err
	}
	checkout := filepath.Join(builds, spec.Project, spec.Repo)
	if operation == "fetch" {
		return fetch(checkout, spec.Path, stdout, stderr)
	}
	return runSpec(builds, checkout, spec, input, stdout, stderr)
}

// buildsDir is ~/Builds, refused when it resolves into ~/Work or is not a real folder.
func buildsDir(home string) (string, error) {
	builds := filepath.Join(home, "Builds")
	if err := os.MkdirAll(builds, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(builds)
	if err != nil || !info.IsDir() {
		return "", errors.New("~/Builds must be a folder, not a link")
	}
	work := filepath.Join(home, "Work")
	if rel, err := filepath.Rel(work, builds); err == nil && filepath.IsLocal(rel) {
		return "", errors.New("~/Builds must not be inside ~/Work")
	}
	return builds, nil
}

func runSpec(builds, checkout string, spec Spec, bundle io.Reader, stdout, stderr io.Writer) error {
	incoming := filepath.Join(builds, ".incoming")
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		return err
	}
	path := filepath.Join(incoming, spec.ID+".bundle")
	defer os.Remove(path)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, err := io.Copy(file, io.LimitReader(bundle, maxBundle+1))
	file.Close()
	if err != nil {
		return err
	}
	if written > maxBundle {
		return errors.New("the job bundle is larger than 4 GiB")
	}
	if err := os.MkdirAll(filepath.Dir(checkout), 0o700); err != nil {
		return err
	}
	steps := [][]string{}
	if _, err := os.Stat(filepath.Join(checkout, ".git")); err != nil {
		steps = append(steps, []string{"init", "-q", checkout})
	}
	// The checkout is kept between jobs, so builds stay incremental. Ignored build output stays,
	// anything else not in the commit goes.
	steps = append(steps,
		[]string{"-C", checkout, "fetch", "-q", "--no-tags", path, "refs/agent-job/" + spec.ID},
		[]string{"-C", checkout, "checkout", "-q", "-f", "--detach", spec.Commit},
		[]string{"-C", checkout, "clean", "-q", "-ffd"})
	for _, step := range steps {
		if output, err := exec.Command("git", step...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", step[len(step)-1], err, strings.TrimSpace(string(output)))
		}
	}
	fmt.Fprintf(stderr, "--- %s at %s in %s\n", spec.Repo, spec.Commit[:12], checkout)
	args := spec.Args
	// Keep the Mac awake while the job runs. A closed lid still sleeps it, then the job runs again.
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/usr/bin/caffeinate"); err == nil {
			args = append([]string{"/usr/bin/caffeinate", "-i"}, args...)
		}
	}
	ctx, stop := context.WithTimeout(context.Background(), runLimit)
	defer stop()
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Dir = checkout
	command.Env = append(os.Environ(), "AGENT_JOB_ID="+spec.ID, "AGENT_JOB_PROJECT="+spec.Project)
	command.Stdin = nil
	command.Stdout, command.Stderr = stdout, stderr
	err = command.Run()
	if ctx.Err() != nil {
		fmt.Fprintf(stderr, "--- the job ran longer than %s and was stopped\n", runLimit)
		return exitCode{124}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		// 255 means a lost connection to the execution host, so a job's own 255 is reported as 254.
		if code == 255 || code < 0 {
			code = 254
		}
		return exitCode{code}
	}
	return err
}

// exitCode is the job's exit code, passed on without a message.
type exitCode struct{ code int }

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// ExitCode reports the code a worker error carries, if any.
func ExitCode(err error) (int, bool) {
	var code exitCode
	if errors.As(err, &code) {
		return code.code, true
	}
	return 0, false
}

// fetch writes a tar of the checkout's path to stdout: folders and regular files only, links skipped.
func fetch(checkout, path string, stdout, stderr io.Writer) error {
	base, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return fmt.Errorf("no checkout for this repository: %v", err)
	}
	target, err := filepath.EvalSymlinks(filepath.Join(base, path))
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	if rel, err := filepath.Rel(base, target); err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("%s leaves the checkout", path)
	}
	writer := tar.NewWriter(stdout)
	parent := filepath.Dir(target)
	err = filepath.WalkDir(target, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(parent, current)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			fmt.Fprintf(stderr, "--- skipped %s, not a file or folder\n", name)
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(name)
		header.Uname, header.Gname = "", ""
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		file, err := os.Open(current)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(writer, file)
		return err
	})
	if err != nil {
		return err
	}
	return writer.Close()
}

// workCleanup runs work-wipe --migrated, which removes only what moved to the work host.
func workCleanup(home, mode string, stdout, stderr io.Writer) error {
	command := exec.Command(filepath.Join(home, ".local/bin/work-wipe"), "--migrated", mode)
	command.Stdin = nil
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exitCode{exit.ExitCode()}
	}
	return err
}
