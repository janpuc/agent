package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	ticketPattern = `[A-Z][A-Z0-9]{1,9}-[0-9]{1,7}`
	slugPattern   = `[a-z0-9][a-z0-9-]{1,39}`
)

var (
	ticketIdentity = regexp.MustCompile(`^` + ticketPattern + `$`)
	slugIdentity   = regexp.MustCompile(`^` + slugPattern + `$`)
	// taskIdentity is a task folder name: a ticket such as GARDEN-12 or a slug such as audit.
	taskIdentity = regexp.MustCompile(`^(?:` + ticketPattern + `|` + slugPattern + `)$`)
)

// ValidTicket reports whether identity is a ticket a task may be named after.
func ValidTicket(identity string) bool { return ticketIdentity.MatchString(identity) }

// ValidSlug reports whether identity is a slug a task may be named after.
func ValidSlug(identity string) bool { return slugIdentity.MatchString(identity) }

// Task is a task's validated metadata. The brief and state files carry the rest.
type Task struct {
	ID          string  `json:"id"`
	Ticket      *string `json:"ticket"`
	Opened      string  `json:"opened"`
	Status      string  `json:"status"`
	FoldRequest string  `json:"foldRequest,omitempty"`
}

// TicketName is the attached ticket, or none when the task has no ticket.
func (t Task) TicketName() string {
	if t.Ticket == nil || *t.Ticket == "" {
		return "none"
	}
	return *t.Ticket
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
	var meta Task
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	if meta.ID != task || (meta.Status != "open" && meta.Status != "closing" && meta.Status != "closed") ||
		!IsDir(path) {
		return nil, fmt.Errorf("%s: invalid task state, restore needed", task)
	}
	return &meta, nil
}

// SaveTask writes a task's metadata.
func SaveTask(folder string, task *Task) error {
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(filepath.Join(TaskState(folder, task.ID), "meta.json"), append(data, '\n'))
}

// UpdateBrief rewrites a task brief's front matter from its metadata. The body is untouched.
func UpdateBrief(folder string, task *Task) error {
	path, err := TaskFolder(folder, task.ID)
	if err != nil {
		return err
	}
	path = filepath.Join(path, "BRIEF.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	rest := strings.TrimPrefix(text, "---\n")
	end := strings.Index(rest, "\n---\n")
	if rest == text || end < 0 {
		return fmt.Errorf("%s: missing task front matter", path)
	}
	body := rest[end+len("\n---\n"):]
	fields := []string{
		"id: " + task.ID,
		"ticket: " + task.TicketName(),
		"opened: " + orNone(task.Opened),
		"status: " + task.Status,
	}
	return AtomicWrite(path, []byte("---\n"+strings.Join(fields, "\n")+"\n---\n"+body))
}

// orNone renders an empty metadata value the way a brief spells it.
func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
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
