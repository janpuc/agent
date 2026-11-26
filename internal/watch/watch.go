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
	"strings"
	"time"

	"github.com/janpuc/agent/internal/project"
	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/rex"
	"github.com/janpuc/agent/internal/workspace"
)

// startInterval is the shortest time between two watchdog starts of one project's runner, so a
// project that keeps dying cannot turn the timer into a loop.
const startInterval = 10 * time.Minute

// Once runs one pass over this host's active projects: it starts every dead runner and continues
// every session whose last turn stalled. Archived projects are never touched. A project that
// fails to start is logged and the pass goes on. It returns an error only when Rex is unreachable,
// in which case nothing is started at all, or when its own records cannot be read or written.
func Once(h project.Host, dryRun bool, out io.Writer) error {
	logger := &log{path: filepath.Join(registry.StateDir(h.Home), "watch.log"), out: out, dry: dryRun}
	if !dryRun {
		if err := os.MkdirAll(registry.StateDir(h.Home), 0o755); err != nil {
			return err
		}
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

// record is what the watchdog remembers about one project between passes.
type record struct {
	// Started is when watch last started this project's runner.
	Started string `json:"started,omitempty"`
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
