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

// Running reports whether a runner holds the workspace lock. The check takes the lock without
// blocking and releases it at once.
func Running(folder string) bool {
	path := filepath.Join(folder, ".agent", "run.lock")
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

// Record is a project's initialization record.
type Record struct {
	SchemaVersion int    `json:"schemaVersion"`
	Project       string `json:"project"`
	Workspace     string `json:"workspace"`
	Origin        string `json:"origin"`
	State         string `json:"state"`
}

// Initialization returns the project's initialization record. It is nil for a project that was
// never initialized, and an error when the record cannot be trusted.
func Initialization(home, root, name string) (*Record, error) {
	path := filepath.Join(registry.StateDir(home), "initialized", name)
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
