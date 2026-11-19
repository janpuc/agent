package tests

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// logActions writes the action log the guard and the commands append to, one object per line.
func (f *fixture) logActions(actions ...map[string]any) {
	f.t.Helper()
	var lines []string
	for _, action := range actions {
		line, err := json.Marshal(action)
		must(f.t, err)
		lines = append(lines, string(line))
	}
	f.write(".local/state/agents/actions.jsonl", strings.Join(lines, "\n")+"\n")
}

func TestDigestReportsCommitsAndRecentActions(t *testing.T) {
	f := newFixture(t, "hub")
	garden := f.open("garden")
	f.write(filepath.Join(garden, "notes/STATE.md"), "# State\n\n## Current work\n\nWater the beds.\n")
	f.git(garden, "commit", "-am", "Record the watered beds")
	now := time.Now()
	f.logActions(
		map[string]any{"ts": now.Add(-time.Hour).UTC().Format(time.RFC3339), "project": "garden",
			"kind": "push", "what": "pushed garden", "command": "git push"},
		map[string]any{"ts": now.Add(-48 * time.Hour).UTC().Format(time.RFC3339), "project": "garden",
			"kind": "deploy", "what": "deployed the old build", "command": "mise run deploy"},
	)
	out := f.ok(f.agent("digest", "24")).stdout
	if !strings.HasPrefix(out, "# hub digest, last 24 hours") {
		t.Fatalf("header\n%s", out)
	}
	for _, want := range []string{"## garden", "- commit garden ", "Record the watered beds",
		"- push pushed garden: git push"} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest lacks %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "deployed the old build") {
		t.Fatalf("digest reports an action older than the window\n%s", out)
	}
}

func TestDigestWithoutHistorySaysNothingHappened(t *testing.T) {
	f := newFixture(t, "hub")
	if out := f.ok(f.agent("digest")).stdout; !strings.Contains(out, "Nothing committed, pushed or overridden.") {
		t.Fatalf("empty digest\n%s", out)
	}
}
