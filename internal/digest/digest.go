// Package digest renders the commits, pushes and overrides of a time window.
package digest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/janpuc/agent/internal/workspace"
)

// Source is what a digest reports on.
type Source struct {
	// Root is the workspace root of this host.
	Root string
	// Host names the machine in the header.
	Host string
	// Projects are the projects on this host, in name order.
	Projects []string
	// Log is the action log, which may be missing.
	Log string
	// Hours is the length of the window.
	Hours int
	// Now is the time the digest is made.
	Now time.Time
}

// Render is the digest text.
func Render(source Source) (string, error) {
	since := source.Now.Add(-time.Duration(source.Hours) * time.Hour)
	actions := actionsSince(source.Log, since)
	out := []string{
		fmt.Sprintf("# %s digest, last %d hours to %s", source.Host, source.Hours,
			source.Now.Local().Format("2006-01-02 15:04 MST")),
		"",
	}
	known := map[string]bool{}
	for _, name := range source.Projects {
		known[name] = true
	}
	var extra []string
	for name := range actions {
		if !known[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range append(append([]string{}, source.Projects...), extra...) {
		block, err := projectBlock(filepath.Join(source.Root, name), name, since, actions[name])
		if err != nil {
			return "", err
		}
		if len(block) > 0 {
			out = append(out, "## "+name)
			out = append(out, block...)
			out = append(out, "")
		}
	}
	if len(out) == 2 {
		out = append(out, "Nothing committed, pushed or overridden.")
	}
	return strings.TrimRight(strings.Join(out, "\n"), " \t\r\n") + "\n", nil
}

// projectBlock is the project's commits per repository followed by its actions.
func projectBlock(folder, name string, since time.Time, actions []logged) ([]string, error) {
	repos := []struct{ label, path string }{{name, folder}}
	if _, err := os.Stat(filepath.Join(folder, ".git")); err == nil {
		// A broken membership file does not stop the digest, the listing reports it.
		if members, err := workspace.Members(folder); err == nil {
			for _, member := range members {
				repos = append(repos, struct{ label, path string }{name + "/" + member.Path,
					filepath.Join(folder, member.Path)})
			}
		}
	}
	var block []string
	for _, repo := range repos {
		if _, err := os.Stat(filepath.Join(repo.path, ".git")); err != nil {
			continue
		}
		command := exec.Command("git", "-C", repo.path, "log", "--branches", "--since="+since.Format(time.RFC3339),
			"--format=%h %s", "-n", "30")
		output, _ := command.Output()
		for _, line := range strings.Split(string(output), "\n") {
			if line != "" {
				block = append(block, fmt.Sprintf("- commit %s %s", repo.label, line))
			}
		}
	}
	for _, action := range actions {
		line := "- " + action.kind + " " + action.what
		if action.failed {
			line += " FAILED"
		}
		if action.reason != "" {
			line += " (reason: " + action.reason + ")"
		}
		block = append(block, line+": "+action.command)
	}
	return block, nil
}

// logged is one action log line that falls inside the window.
type logged struct {
	at      time.Time
	project string
	kind    string
	what    string
	command string
	reason  string
	failed  bool
}

// actionsSince groups the action log lines by project. A missing log has no actions.
func actionsSince(path string, window time.Time) map[string][]logged {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	grouped := map[string][]logged{}
	for _, line := range strings.Split(string(data), "\n") {
		action, ok := parseAction(line)
		if !ok || action.at.Before(window) {
			continue
		}
		grouped[action.project] = append(grouped[action.project], action)
	}
	return grouped
}

// parseAction decodes one action log line. Malformed lines are ignored.
func parseAction(line string) (logged, bool) {
	var entry map[string]any
	if json.Unmarshal([]byte(line), &entry) != nil {
		return logged{}, false
	}
	stamp, ok := entry["ts"].(string)
	if !ok {
		return logged{}, false
	}
	at, err := parseTime(stamp)
	if err != nil {
		return logged{}, false
	}
	what := text(entry["what"])
	if what == "" {
		return logged{}, false
	}
	action := logged{at: at, project: "-", kind: "action", what: what, command: text(entry["command"])}
	if value := text(entry["project"]); value != "" {
		action.project = value
	}
	if value := text(entry["kind"]); value != "" {
		action.kind = value
	}
	if value := text(entry["reason"]); value != "" {
		action.reason = value
	}
	if failed, ok := entry["ok"].(bool); ok && !failed {
		action.failed = true
	}
	return action, true
}

// parseTime accepts the UTC and the naive timestamps the log writers use.
func parseTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
		if at, err := time.Parse(layout, value); err == nil {
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %s", value)
}

// text renders a log field, empty when it is unset.
func text(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	default:
		return fmt.Sprint(value)
	}
}
