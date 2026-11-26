// Package watch is the one pass watchdog that runs outside Rex, so it cannot die with a Rex
// restart. It starts the runner of a project whose tab died and continues a session whose last
// turn ended on a provider stall.
package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/janpuc/agent/internal/project"
	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/requests"
	"github.com/janpuc/agent/internal/rex"
	"github.com/janpuc/agent/internal/runner"
	"github.com/janpuc/agent/internal/workspace"
)

const (
	// startInterval is the shortest time between two watchdog starts of one project's runner, so a
	// project that keeps dying cannot turn the timer into a loop.
	startInterval = 10 * time.Minute
	// stallAge is how long a session file must sit unchanged after a stalled turn before its
	// continue is sent, so a turn that is merely slow is never doubled.
	stallAge = 5 * time.Minute
)

// Once runs one pass over this host's schedules, then over its active projects: it starts every
// dead runner and continues every session whose last turn stalled. Archived projects are never
// touched. A project that fails to start is logged and the pass goes on. It returns an error only
// when Rex is unreachable, in which case nothing is started at all, or when its own records cannot
// be read or written.
func Once(h project.Host, dryRun bool, out io.Writer) error {
	logger := &log{path: filepath.Join(registry.StateDir(h.Home), "watch.log"), out: out, dry: dryRun}
	if !dryRun {
		if err := os.MkdirAll(registry.StateDir(h.Home), 0o755); err != nil {
			return err
		}
	}
	// A schedule only writes an inbox file, so it runs whether or not Rex answers.
	if err := runSchedules(h, logger, dryRun); err != nil {
		return err
	}
	if _, err := rex.Sessions(); err != nil {
		return logger.line("rex unreachable, starting nothing: %v", err)
	}
	var failed []string
	for _, name := range h.Registry.Names(h.Name) {
		lifecycle, err := h.Registry.Lifecycle(h.Home, name)
		if err != nil {
			return err
		}
		if lifecycle != "active" {
			continue
		}
		record, err := readRecord(h.Home, name)
		if err != nil {
			return err
		}
		if err := start(h, name, record, logger, dryRun); err != nil {
			failed = append(failed, name)
		}
		if err := continueStalled(h, name, record, logger, dryRun); err != nil {
			return err
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("watch could not start %s", strings.Join(failed, ", "))
	}
	return nil
}

// start opens a project whose runner is dead, at most once per startInterval. A live runner is
// left alone.
func start(h project.Host, name string, record *record, logger *log, dryRun bool) error {
	if workspace.Running(filepath.Join(h.Root, name)) {
		return nil
	}
	now := time.Now()
	if record.Started != "" {
		if started, err := time.Parse(time.RFC3339, record.Started); err == nil && now.Sub(started) < startInterval {
			return logger.line("%s: runner dead, started %s, inside the %s interval", name,
				started.UTC().Format(time.RFC3339), startInterval)
		}
	}
	if dryRun {
		return logger.line("%s: runner dead, would start it", name)
	}
	if err := h.Ensure(name); err != nil {
		logger.line("%s: start failed: %v", name, err)
		return err
	}
	record.Started = now.UTC().Format(time.RFC3339)
	if err := record.save(h.Home, name); err != nil {
		return err
	}
	return logger.line("%s: runner started", name)
}

// continueStalled sends one continue into every session of a project whose last turn ended on a
// provider stall and that has not changed since the last continue. The message goes into the queue
// the session's own extension polls, so nothing is typed into a running tab.
func continueStalled(h project.Host, name string, record *record, logger *log, dryRun bool) error {
	stalls, err := stalls(h, name)
	if err != nil {
		return err
	}
	for _, stall := range stalls {
		if record.Continued[stall.Path] == stall.Stamp {
			continue
		}
		if dryRun {
			if err := logger.line("%s: %s stalled on %q, would continue it", name, stall.what(), stall.Text); err != nil {
				return err
			}
			continue
		}
		if err := queueContinue(h, name, stall); err != nil {
			if err := logger.line("%s: could not continue %s: %v", name, stall.what(), err); err != nil {
				return err
			}
			continue
		}
		if record.Continued == nil {
			record.Continued = map[string]string{}
		}
		record.Continued[stall.Path] = stall.Stamp
		if err := record.save(h.Home, name); err != nil {
			return err
		}
		if err := logger.line("%s: continued %s after %q", name, stall.what(), stall.Text); err != nil {
			return err
		}
	}
	return nil
}

// stall is one session whose last turn ended on a provider stall.
type stall struct {
	// Path is the session file, the key the record keeps its stamp under.
	Path string
	// Task is the task the session runs, empty for the project session.
	Task string
	// Text is the error the turn ended with.
	Text string
	// Stamp is the session file's size and modification time.
	Stamp string
}

// what names the session in a log line.
func (s stall) what() string {
	if s.Task == "" {
		return "the project session"
	}
	return "task " + s.Task
}

// stalls lists the sessions of one project that ended their last turn on a provider stall and have
// not changed for stallAge.
func stalls(h project.Host, name string) ([]stall, error) {
	folder := filepath.Join(h.Root, name)
	type candidate struct{ path, task string }
	var candidates []candidate
	generation, err := workspace.Generation(folder)
	if err != nil {
		return nil, err
	}
	path, err := runner.SessionFile(runner.SessionDir(h.Home, folder), name+"-"+strconv.Itoa(generation))
	if err != nil {
		return nil, err
	}
	if path != "" {
		candidates = append(candidates, candidate{path: path})
	}
	tasks, err := workspace.TaskRecords(folder)
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		if task.Status == "closed" {
			continue
		}
		taskFolder, err := workspace.TaskFolder(folder, task.ID)
		if err != nil || !workspace.IsDir(taskFolder) {
			continue
		}
		path, err := runner.SessionFile(runner.SessionDir(h.Home, taskFolder), name+"-task-"+task.ID)
		if err != nil {
			return nil, err
		}
		if path != "" {
			candidates = append(candidates, candidate{path: path, task: task.ID})
		}
	}
	now := time.Now()
	var found []stall
	for _, session := range candidates {
		info, err := os.Stat(session.path)
		if err != nil || now.Sub(info.ModTime()) < stallAge {
			continue
		}
		text, err := stallText(session.path)
		if err != nil {
			return nil, err
		}
		if text == "" {
			continue
		}
		found = append(found, stall{Path: session.path, Task: session.task, Text: text,
			Stamp: fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())})
	}
	return found, nil
}

// queueContinue puts a continue in the queue the session's extension polls when Pi is idle: a
// task's messages folder, or the project's inbox, which hands it over the same way.
func queueContinue(h project.Host, name string, stall stall) error {
	folder := filepath.Join(h.Root, name)
	text := continueText(name, stall.Text)
	if stall.Task != "" {
		box := filepath.Join(workspace.TaskState(folder, stall.Task), "messages")
		if err := os.MkdirAll(box, 0o700); err != nil {
			return err
		}
		return workspace.AtomicWrite(
			filepath.Join(box, time.Now().UTC().Format("20060102T150405.000000000Z")+".md"), []byte(text+"\n"))
	}
	id, err := requests.NewID()
	if err != nil {
		return err
	}
	// The item is the project's own, so the inbox hands it over as a fold and asks no reply.
	item := map[string]string{
		"id": id, "from": name, "to": name, "text": text,
		"created": requests.Timestamp(), "status": "new",
	}
	data, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return err
	}
	box := filepath.Join(folder, ".agent/inbox")
	if err := os.MkdirAll(box, 0o755); err != nil {
		return err
	}
	return workspace.AtomicWrite(filepath.Join(box, id+".json"), append(data, '\n'))
}

// continueText is the message a stalled session receives. It names its source, so the agent never
// mistakes it for the operator's instruction.
func continueText(name, failure string) string {
	return fmt.Sprintf("Platform watchdog: your last turn in %s ended with a provider error and "+
		"nothing has run since. The error was: %s. Nobody is typing. Continue the work from where it "+
		"stopped, and read the state you left on disk before acting. If the same call stalls again, "+
		"report what blocks it instead of retrying it.", name, failure)
}

// stallText is the error a session's last turn ended with, empty when that turn did not end on a
// provider stall.
func stallText(path string) (string, error) {
	line, err := lastMessage(path)
	if err != nil || line == nil {
		return "", err
	}
	var entry struct {
		Message struct {
			Role         string `json:"role"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &entry); err != nil {
		return "", nil
	}
	if entry.Message.Role != "assistant" || entry.Message.StopReason != "error" ||
		!strings.Contains(strings.ToLower(entry.Message.ErrorMessage), "stalled") {
		return "", nil
	}
	return entry.Message.ErrorMessage, nil
}

// lastMessage is the last message entry of a session file, nil when there is none. It reads from
// the end of the file, so a long session is never loaded whole, and grows the window until the
// entry it lands on is one complete JSON line.
func lastMessage(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	for window := int64(64 << 10); ; window *= 8 {
		start := size - window
		if start < 0 {
			start = 0
		}
		buffer := make([]byte, size-start)
		if _, err := file.ReadAt(buffer, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		text := strings.TrimSuffix(string(buffer), "\n")
		if start > 0 {
			// The window starts inside a line, so that line is dropped.
			index := strings.Index(text, "\n")
			if index < 0 {
				text = ""
			} else {
				text = text[index+1:]
			}
		}
		lines := strings.Split(text, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := []byte(lines[i])
			var entry struct {
				Type    string `json:"type"`
				Message struct {
					Role string `json:"role"`
				} `json:"message"`
			}
			if err := json.Unmarshal(line, &entry); err != nil {
				continue
			}
			if entry.Type == "message" && entry.Message.Role == "assistant" {
				return line, nil
			}
		}
		if start == 0 {
			return nil, nil
		}
	}
}

// record is what the watchdog remembers about one project between passes.
type record struct {
	// Started is when watch last started this project's runner.
	Started string `json:"started,omitempty"`
	// Continued maps a session file to the stamp it had when watch last continued it.
	Continued map[string]string `json:"continued,omitempty"`
}

// recordPath is the project's watchdog record file.
func recordPath(home, name string) string {
	return filepath.Join(registry.StateDir(home), "watch", name+".json")
}

// readRecord reads a project's record, an empty one when watch has not acted yet.
func readRecord(home, name string) (*record, error) {
	value := &record{}
	data, err := os.ReadFile(recordPath(home, name))
	if errors.Is(err, os.ErrNotExist) {
		return value, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return nil, fmt.Errorf("invalid watchdog record %s: %w", recordPath(home, name), err)
	}
	return value, nil
}

// save writes the project's record.
func (r *record) save(home, name string) error {
	path := recordPath(home, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return workspace.AtomicWrite(path, append(data, '\n'))
}

// log writes one line per action to the watchdog's state log and to out. A dry run leaves the log
// alone, so it changes nothing.
type log struct {
	path string
	out  io.Writer
	dry  bool
}

// line records one action.
func (l *log) line(format string, args ...any) error {
	text := fmt.Sprintf(format, args...)
	if !l.dry {
		file, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(file, "%s %s\n", time.Now().UTC().Format(time.RFC3339), text)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(l.out, text)
	return err
}
