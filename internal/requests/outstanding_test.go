package requests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// put writes one inbox record for project below root.
func put(t *testing.T, root, project, id string, fields map[string]any) {
	t.Helper()
	box := filepath.Join(root, project, ".agent/inbox")
	if err := os.MkdirAll(box, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(box, id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOutstandingDerivesNeedsJanItems checks the derivation the needs-Jan list depends on: an
// answered request leaves the list, a request only appears once it is a day old, and only a
// needs-jan reply brings the operator into the chain.
func TestOutstandingDerivesNeedsJanItems(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	at := func(hours int) string { return now.Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339) }
	put(t, root, "kubernetes", "20261120T120000-111111", map[string]any{
		"id": "20261120T120000-111111", "from": "main", "to": "kubernetes", "status": "accepted",
		"text": "Rotate the credentials", "created": at(150)})
	put(t, root, "kubernetes", "20261119T093954-8102f4", map[string]any{
		"id": "20261119T093954-8102f4", "from": "main", "to": "kubernetes", "status": "accepted",
		"text": "Rotate the two cluster-side credentials", "created": at(168)})
	put(t, root, "kubernetes", "20261126T110000-aaaaaa", map[string]any{
		"id": "20261126T110000-aaaaaa", "from": "main", "to": "kubernetes", "status": "new",
		"text": "Fresh request", "created": at(1)})
	put(t, root, "dotfiles", "20261124T100000-bbbbbb", map[string]any{
		"id": "20261124T100000-bbbbbb", "from": "garden", "to": "dotfiles", "status": "accepted",
		"text": "Answered request", "created": at(48)})
	put(t, root, "garden", "20261124T110000-cccccc", map[string]any{
		"id": "20261124T110000-cccccc", "from": "dotfiles", "to": "garden", "status": "new",
		"text": "Done", "created": at(47), "inReplyTo": "20261124T100000-bbbbbb", "outcome": "done"})
	put(t, root, "kubernetes", "20261125T090000-dddddd", map[string]any{
		"id": "20261125T090000-dddddd", "from": "kubernetes", "to": "main", "status": "new",
		"text": "Which key goes in 1Password", "created": at(27),
		"inReplyTo": "20261119T093954-8102f4", "outcome": "needs-jan"})
	put(t, root, "dotfiles", "20261125T100000-eeeeee", map[string]any{
		"id": "20261125T100000-eeeeee", "from": "dotfiles", "to": "work", "status": "new",
		"text": "A needs-jan reply that Jan answered", "created": at(26),
		"inReplyTo": "20261123T071634-98f8e9", "outcome": "needs-jan"})
	put(t, root, "dotfiles", "20261125T110000-ffffff", map[string]any{
		"id": "20261125T110000-ffffff", "from": "dotfiles", "to": "dotfiles", "status": "new",
		"text": "The answer Jan gave", "created": at(25),
		"inReplyTo": "20261125T100000-eeeeee", "outcome": "done"})
	put(t, root, "kubernetes", "20261122T180039-867aa5", map[string]any{
		"id": "20261122T180039-867aa5", "from": "kubernetes", "to": "kubernetes", "kind": "fold",
		"status": "accepted", "text": "Fold task stale-alerts", "created": at(90)})

	got := Outstanding(root, []string{"dotfiles", "garden", "kubernetes"}, "jan", now)
	if len(got) != 2 {
		t.Fatalf("outstanding %v", got)
	}
	old := got[0]
	if old.Project != "kubernetes" || old.Request != "111111" || !old.Overdue ||
		old.Chain != "main waits on kubernetes" || old.Text != "Rotate the credentials" ||
		!old.Created.Equal(now.Add(-150*time.Hour)) {
		t.Fatalf("open request %v", old)
	}
	asking := got[1]
	if asking.Project != "kubernetes" || asking.Request != "8102f4" || asking.Overdue ||
		asking.Chain != "kubernetes waits on jan" || asking.Text != "Which key goes in 1Password" {
		t.Fatalf("needs-jan reply %v", asking)
	}
}
