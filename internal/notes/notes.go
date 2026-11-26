// Package notes reads project knowledge files.
package notes

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PendingApprovals returns the bullets under STATE.md's Pending approvals heading, with
// indented continuation lines joined. A missing file has none.
func PendingApprovals(workspace string) []string {
	data, err := os.ReadFile(filepath.Join(workspace, "notes/STATE.md"))
	if err != nil {
		return nil
	}
	var questions []string
	inside := false
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			inside = strings.EqualFold(strings.TrimSpace(line[3:]), "pending approvals")
		case inside && strings.HasPrefix(line, "- "):
			questions = append(questions, strings.TrimSpace(line[2:]))
		case inside && strings.HasPrefix(line, "  ") && len(questions) > 0:
			questions[len(questions)-1] += " " + strings.TrimSpace(line)
		}
	}
	return questions
}

// StateMtime is when the project's STATE.md was last written. A question has no timestamp of its
// own, and a git log search for each bullet would cost a git process per line, so the file's
// mtime dates every question in it.
func StateMtime(workspace string) (time.Time, bool) {
	info, err := os.Stat(filepath.Join(workspace, "notes/STATE.md"))
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}
