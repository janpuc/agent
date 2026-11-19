// Package workspace reads the state of a project workspace: generation, runner lock,
// initialization record, repositories and tasks.
package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/janpuc/agent/internal/registry"
)

// Generation is the project's current generation. A workspace without the file is generation 1.
func Generation(folder string) (int, error) {
	path := filepath.Join(folder, ".agent", "generation")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || value < 1 {
		return 0, fmt.Errorf("invalid generation in %s", path)
	}
	return value, nil
}

// Running reports whether a runner holds the project's workspace lock. The check takes the lock
// without blocking and releases it at once.
func Running(folder string) bool {
	return runningAt(filepath.Join(folder, ".agent"))
}

// RunningTask reports whether a runner holds a task's lock.
func RunningTask(folder, task string) bool {
	return runningAt(TaskState(folder, task))
}

// runningAt reports whether a live runner holds the lock in a state directory.
func runningAt(state string) bool {
	path := filepath.Join(state, "run.lock")
	if _, err := os.Stat(path); err != nil {
		return false
	}
	lock, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return false
	}
	defer lock.Close()
	return syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == syscall.EWOULDBLOCK
}

// RunnerPID is the process id the runner wrote into the workspace lock, the evidence archive
// needs before it stops a tab.
func RunnerPID(folder string) (int, error) {
	data, err := os.ReadFile(filepath.Join(folder, ".agent", "run.lock"))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	return pid, nil
}

// ProjectLock opens the platform lock that serialises lifecycle commands for a project.
func ProjectLock(home, name string) (*os.File, error) {
	path := filepath.Join(registry.StateDir(home), "locks", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// AtomicWrite writes a temporary file beside path and renames it over path.
func AtomicWrite(path string, data []byte) error {
	temp := path + ".tmp"
	defer os.Remove(temp)
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// Exists reports whether path exists, following symlinks.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsSymlink reports whether path itself is a symlink.
func IsSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// IsDir reports whether path exists and is a directory, following symlinks.
func IsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Resolve follows every existing symlink in path, including below a missing component, the way
// Path.resolve() does for a path that does not exist yet.
func Resolve(path string) string {
	path = filepath.Clean(path)
	if target, err := filepath.EvalSymlinks(path); err == nil {
		return target
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(Resolve(parent), filepath.Base(path))
}

// Record is a project's initialization record.
type Record struct {
	SchemaVersion int    `json:"schemaVersion"`
	Project       string `json:"project"`
	Workspace     string `json:"workspace"`
	Origin        string `json:"origin"`
	State         string `json:"state"`
}

// InitializationPath is the platform record that says a project was bootstrapped or adopted.
func InitializationPath(home, name string) string {
	return filepath.Join(registry.StateDir(home), "initialized", name)
}

// RecordInitialization writes the project's initialization record.
func RecordInitialization(home, root, name, origin, state string) error {
	path := InitializationPath(home, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	record := Record{SchemaVersion: 1, Project: name, Workspace: filepath.Join(root, name),
		Origin: origin, State: state}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(path, append(data, '\n'))
}

// Initialization returns the project's initialization record. It is nil for a project that was
// never initialized, and an error when the record cannot be trusted.
func Initialization(home, root, name string) (*Record, error) {
	path := InitializationPath(home, name)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record Record
	valid := json.Unmarshal(data, &record) == nil && record.SchemaVersion == 1 &&
		record.Project == name && record.Workspace == filepath.Join(root, name) &&
		(record.Origin == "bootstrap" || record.Origin == "adopted") &&
		(record.State == "initializing" || record.State == "initialized")
	if !valid {
		return nil, fmt.Errorf("%s: invalid initialization record, restore needed: %s", name, path)
	}
	return &record, nil
}
