// Package actions appends to the platform action log, the log the guard writes and the digest
// reads.
package actions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// entry is one action log line.
type entry struct {
	TS      string `json:"ts"`
	Host    string `json:"host"`
	Project any    `json:"project"`
	Kind    string `json:"kind"`
	What    string `json:"what"`
	Command string `json:"command"`
}

// Append logs one action. A log failure never stops the command.
func Append(home, host, what, detail string) {
	var project any
	if value, ok := os.LookupEnv("PI_PROJECT"); ok {
		project = value
	}
	line, err := json.Marshal(entry{
		TS:      time.Now().UTC().Format("2006-01-02T15:04:05") + "+00:00",
		Host:    host,
		Project: project,
		Kind:    "action",
		What:    what,
		Command: truncate(detail, 300),
	})
	if err != nil {
		return
	}
	path := filepath.Join(home, ".local/state/agents/actions.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	file.Write(append(line, '\n'))
}

// truncate keeps the first limit characters, as the Python runner slices the string.
func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
