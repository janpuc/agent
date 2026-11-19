package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// taskIdentity is a task folder name: a ticket such as GARDEN-12 or a slug such as audit.
var taskIdentity = regexp.MustCompile(`^(?:[A-Z][A-Z0-9]{1,9}-[0-9]{1,7}|[a-z0-9][a-z0-9-]{1,39})$`)

// Task is a task's validated metadata. The brief and state files carry the rest.
type Task struct {
	ID     string
	Status string
}

// TaskState is the directory of a task's runtime state.
func TaskState(folder, task string) string {
	return filepath.Join(folder, ".agent", "tasks", task)
}

// TaskFolder is a task's working folder, checked so it cannot escape the workspace.
func TaskFolder(folder, task string) (string, error) {
	if !taskIdentity.MatchString(task) {
		return "", fmt.Errorf("Invalid task identity: %s", task)
	}
	path := filepath.Join(folder, "tasks", task)
	if IsSymlink(path) || filepath.Dir(Resolve(path)) != filepath.Join(Resolve(folder), "tasks") ||
		IsSymlink(filepath.Join(folder, "tasks")) {
		return "", fmt.Errorf("Task path escapes workspace: %s", path)
	}
	state := TaskState(folder, task)
	if IsSymlink(state) || filepath.Dir(Resolve(state)) != filepath.Join(Resolve(folder), ".agent", "tasks") {
		return "", fmt.Errorf("Task state escapes workspace: %s", state)
	}
	return path, nil
}

// ReadTask reads and validates one task's metadata.
func ReadTask(folder, task string) (*Task, error) {
	path, err := TaskFolder(folder, task)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(TaskState(folder, task), "meta.json"))
	if err != nil {
		return nil, err
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	id, _ := meta["id"].(string)
	status, _ := meta["status"].(string)
	if id != task || (status != "open" && status != "closing" && status != "closed") || !IsDir(path) {
		return nil, fmt.Errorf("%s: invalid task state, restore needed", task)
	}
	return &Task{ID: id, Status: status}, nil
}

// TaskRecords reads every task's metadata in name order.
func TaskRecords(folder string) ([]Task, error) {
	entries, err := os.ReadDir(filepath.Join(folder, ".agent", "tasks"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tasks []Task
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := ReadTask(folder, entry.Name())
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, *meta)
	}
	return tasks, nil
}

// TaskStatuses lists the statuses in a workspace's task state, in name order.
func TaskStatuses(folder string) ([]string, error) {
	tasks, err := TaskRecords(folder)
	if err != nil {
		return nil, err
	}
	var statuses []string
	for _, task := range tasks {
		statuses = append(statuses, task.Status)
	}
	return statuses, nil
}
