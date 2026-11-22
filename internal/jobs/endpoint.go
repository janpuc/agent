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
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// EndpointUsage lists what the worker endpoint accepts.
const EndpointUsage = "worker endpoint accepts only ping, job, fetch, work-cleanup --dry-run or work-cleanup --really <inventory>"

const (
	// marker marks a checkout this endpoint created. Any other folder in its place is refused.
	marker     = "agent-worker-checkout"
	keepRecord = 14 * 24 * time.Hour
	followPoll = 500 * time.Millisecond
)

var inventoryPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Endpoint is a worker's forced SSH command for the execution host's key. authorized_keys is the
// gate. It runs committed scripts in ~/Builds/<project>/<repo>, returns the files a job kept,
// and runs the migrated Work cleanup, nothing else.
func Endpoint(home string, stdin io.Reader, stdout, stderr io.Writer) error {
	operation := os.Getenv("SSH_ORIGINAL_COMMAND")
	switch {
	case operation == "ping":
		fmt.Fprintln(stdout, "ok")
		return nil
	case operation == "work-cleanup --dry-run":
		return workCleanup(home, []string{"--dry-run"}, stdout, stderr)
	case strings.HasPrefix(operation, "work-cleanup --really "):
		inventory := strings.TrimPrefix(operation, "work-cleanup --really ")
		if !inventoryPattern.MatchString(inventory) {
			return errors.New(EndpointUsage)
		}
		return workCleanup(home, []string{"--really", "--inventory", inventory}, stdout, stderr)
	case operation == "job", operation == "fetch":
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
	builds := filepath.Join(home, "Builds")
	records := filepath.Join(builds, ".jobs")
	for _, folder := range []string{builds, records, filepath.Join(builds, spec.Project)} {
		if err := realDir(folder); err != nil {
			return err
		}
	}
	record := filepath.Join(records, spec.ID)
	if operation == "fetch" {
		return fetch(record, spec.Path, stdout, stderr)
	}
	// A build outlives a lost connection: it writes to its record, not to this session. Notify
	// rather than Ignore, so the build's own processes keep the default signal handling.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE, syscall.SIGHUP)
	prune(records)
	if err := realDir(record); err != nil {
		return err
	}
	return job(builds, record, spec, input, stdout, stderr)
}

// realDir makes path a folder if it is missing and refuses anything that is not a plain folder.
func realDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s must be a folder, not a link or a file", path)
	}
	return nil
}

// prune removes the log, kept files and leftovers of job records older than two weeks that
// nothing runs. The exit code stays for good, so a very late reconnect never builds again.
func prune(records string) {
	entries, err := os.ReadDir(records)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(records, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || !idPattern.MatchString(entry.Name()) || time.Since(info.ModTime()) < keepRecord {
			continue
		}
		unlock, ok := tryLock(filepath.Join(path, "run.lock"))
		if !ok {
			continue
		}
		if _, done := finished(path); done {
			files, _ := os.ReadDir(path)
			for _, file := range files {
				if name := file.Name(); name != "exit" && name != "run.lock" {
					os.RemoveAll(filepath.Join(path, name))
				}
			}
		}
		unlock()
	}
}

// finished returns the recorded exit code of a job that ran to the end.
func finished(record string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(record, "exit"))
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return code, err == nil
}

// tryLock takes an exclusive lock without waiting.
func tryLock(path string) (func(), bool) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, false
	}
	if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		file.Close()
		return nil, false
	}
	return func() { file.Close() }, true
}

func lock(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() { file.Close() }, nil
}

// job runs a job once. A connection for a job that already finished replays its log and exit
// code, and one for a job still running from a lost connection follows it to the end.
func job(builds, record string, spec Spec, input io.Reader, stdout, stderr io.Writer) error {
	logPath := filepath.Join(record, "log")
	var offset int64
	for {
		if code, ok := finished(record); ok {
			io.Copy(io.Discard, input)
			followFrom(logPath, offset, stdout, func() bool { return true })
			return exitCode{code}
		}
		unlock, ok := tryLock(filepath.Join(record, "run.lock"))
		if !ok {
			// The earlier connection's run goes on. Follow its log until it records the result.
			offset = followFrom(logPath, offset, stdout, func() bool {
				if _, done := finished(record); done {
					return true
				}
				free, ok := tryLock(filepath.Join(record, "run.lock"))
				if ok {
					free()
				}
				return ok
			})
			continue
		}
		// The earlier run may have recorded its exit and let go just before this lock was taken.
		if _, done := finished(record); done {
			unlock()
			continue
		}
		defer unlock()
		break
	}
	bundle, err := receive(record, input)
	if bundle != "" {
		defer os.Remove(bundle)
	}
	if err != nil {
		return err
	}
	// One job at a time per checkout, so a retry never builds over a running one.
	release, err := lock(filepath.Join(builds, spec.Project, "."+spec.Repo+".lock"))
	if err != nil {
		return err
	}
	defer release()
	checkout := filepath.Join(builds, spec.Project, spec.Repo)
	if err := prepare(checkout, bundle, spec); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	start, _ := logFile.Seek(0, io.SeekEnd)
	done := make(chan struct{})
	streamed := make(chan struct{})
	go func() {
		followFrom(logPath, start, stdout, func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		})
		close(streamed)
	}()
	fmt.Fprintf(logFile, "--- %s at %s in %s\n", spec.Repo, spec.Commit[:12], checkout)
	code := execute(checkout, spec, logFile)
	if code == 0 {
		keep(checkout, filepath.Join(record, "artifacts"), spec.Fetch, logFile)
	}
	if err := writeAtomic(filepath.Join(record, "exit"), []byte(strconv.Itoa(code)+"\n")); err != nil {
		fmt.Fprintf(logFile, "--- cannot record the exit code: %v\n", err)
	}
	close(done)
	<-streamed
	return exitCode{code}
}

// receive stores the git bundle from stdin in an exclusive temporary file of the record.
func receive(record string, input io.Reader) (string, error) {
	file, err := os.CreateTemp(record, "bundle-*")
	if err != nil {
		return "", err
	}
	written, err := io.Copy(file, io.LimitReader(input, maxBundle+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil && written > maxBundle {
		err = errors.New("the job bundle is larger than 4 GiB")
	}
	return file.Name(), err
}

// prepare checks out the commit in a checkout this endpoint created and checks that the job's
// script is an executable file tracked in that commit.
func prepare(checkout, bundle string, spec Spec) error {
	gitDir := filepath.Join(checkout, ".git")
	if _, err := os.Lstat(checkout); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(checkout, 0o700); err != nil {
			return err
		}
		if err := git(checkout, "init", "-q"); err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(gitDir, marker), []byte("created by agent --worker-endpoint\n")); err != nil {
			return err
		}
	}
	if err := realDir(checkout); err != nil {
		return err
	}
	if info, err := os.Lstat(filepath.Join(gitDir, marker)); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s was not created by the worker endpoint, move it away first", checkout)
	}
	// The checkout is kept between jobs, so builds stay incremental. Ignored build output stays,
	// anything else not in the commit goes.
	steps := [][]string{
		{"fetch", "-q", "--no-tags", bundle, "refs/agent-job/" + spec.ID},
		{"checkout", "-q", "-f", "--detach", spec.Commit},
		{"clean", "-q", "-ffd"},
	}
	for _, step := range steps {
		if err := git(checkout, step...); err != nil {
			return err
		}
	}
	script := strings.TrimPrefix(spec.Args[0], "./")
	tree, err := exec.Command("git", "-C", checkout, "ls-tree", spec.Commit, "--", script).Output()
	if err != nil || !strings.HasPrefix(string(tree), "100755 blob ") {
		return fmt.Errorf("%s is not an executable file committed at %s", spec.Args[0], spec.Commit[:12])
	}
	base, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(checkout, script))
	info, statErr := os.Lstat(filepath.Join(checkout, script))
	if err != nil || statErr != nil || !info.Mode().IsRegular() || resolved != filepath.Join(base, script) {
		return fmt.Errorf("%s must be a plain file in the checkout, not reached through a link", spec.Args[0])
	}
	return nil
}

func git(dir string, args ...string) error {
	command := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return nil
}

// execute runs the job's script with its output in the log and returns its exit code.
func execute(checkout string, spec Spec, logFile *os.File) int {
	args := spec.Args
	// Keep the Mac awake while the job runs. A closed lid still sleeps it, then the job resumes.
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
	command.Stdout, command.Stderr = logFile, logFile
	err := command.Run()
	if ctx.Err() != nil {
		fmt.Fprintf(logFile, "--- the job ran longer than %s and was stopped\n", runLimit)
		return 124
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		// 255 means a lost connection to the execution host, so a job's own 255 is reported as 254.
		if code == 255 || code < 0 {
			code = 254
		}
		return code
	}
	if err != nil {
		fmt.Fprintf(logFile, "--- cannot start %s: %v\n", spec.Args[0], err)
		return 127
	}
	return 0
}

// followFrom streams a log from offset until done reports true and the log has no more bytes,
// and returns the offset it reached. A failed write ends the stream but never the job.
func followFrom(path string, offset int64, out io.Writer, done func() bool) int64 {
	buffer := make([]byte, 32<<10)
	for {
		last := done()
		if file, err := os.Open(path); err == nil {
			file.Seek(offset, io.SeekStart)
			for {
				n, err := file.Read(buffer)
				if n > 0 {
					if _, werr := out.Write(buffer[:n]); werr != nil {
						file.Close()
						return offset
					}
					offset += int64(n)
				}
				if err != nil {
					break
				}
			}
			file.Close()
		}
		if last {
			return offset
		}
		time.Sleep(followPoll)
	}
}

// keep copies the job's --fetch paths from the checkout into the record, so a later fetch returns
// them even after another job changed the checkout. Links are skipped.
func keep(checkout, artifacts string, paths []string, logFile io.Writer) {
	// A run cut off before it recorded its exit code may have kept some files already.
	os.RemoveAll(artifacts)
	base, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return
	}
	for _, path := range paths {
		source, err := filepath.EvalSymlinks(filepath.Join(base, path))
		if err == nil && source != filepath.Join(base, path) {
			err = errors.New("reached through a link")
		}
		if err == nil {
			err = copyTree(source, filepath.Join(artifacts, path), logFile)
		}
		if err != nil {
			fmt.Fprintf(logFile, "--- cannot keep %s: %v\n", path, err)
		}
	}
}

func copyTree(source, target string, logFile io.Writer) error {
	return filepath.WalkDir(source, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Git metadata never leaves the worker, in any letter case since macOS volumes ignore it.
		if strings.EqualFold(entry.Name(), ".git") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.MkdirAll(destination, 0o700)
		case info.Mode().IsRegular():
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			in, err := os.Open(current)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()&0o700|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, in)
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			return err
		}
		fmt.Fprintf(logFile, "--- skipped %s, not a file or folder\n", rel)
		return nil
	})
}

// fetch writes a tar of one kept path to stdout. Names stay relative to the checkout, so two
// paths with the same base name never collide.
func fetch(record, path string, stdout, stderr io.Writer) error {
	if info, err := os.Lstat(record); err != nil || !info.IsDir() {
		return errors.New("no record of this job")
	}
	artifacts := filepath.Join(record, "artifacts")
	if info, err := os.Lstat(artifacts); err != nil || !info.IsDir() {
		return errors.New("this job kept no files")
	}
	target := filepath.Join(artifacts, path)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("%s was not kept: %v", path, err)
	}
	if resolved != target {
		return fmt.Errorf("%s is reached through a link", path)
	}
	writer := tar.NewWriter(stdout)
	err = filepath.WalkDir(target, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(artifacts, current)
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
		header.Name, header.Uname, header.Gname = filepath.ToSlash(name), "", ""
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

func writeAtomic(path string, data []byte) error {
	temp := path + ".tmp"
	defer os.Remove(temp)
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
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

// workCleanup runs work-wipe --migrated, which removes only what moved to the work host. The real
// run names the inventory its dry run printed and refuses a changed one.
func workCleanup(home string, mode []string, stdout, stderr io.Writer) error {
	command := exec.Command(filepath.Join(home, ".local/bin/work-wipe"), append([]string{"--migrated"}, mode...)...)
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exitCode{exit.ExitCode()}
	}
	return err
}
