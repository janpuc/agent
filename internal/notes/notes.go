// Package notes reads project knowledge files.
package notes

import (
	"os"
	"path/filepath"
	"strings"
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
