// Package requests reads the requests waiting in a project's inbox.
package requests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Request is one request record. An inbox file may carry other fields.
type Request struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"`
	Text   string `json:"text"`
}

// Open returns the requests in inbox that are new and addressed to project, oldest file first.
// Unreadable or malformed records are skipped, as a request is never lost by them.
func Open(inbox, project string) ([]Request, error) {
	info, err := os.Stat(inbox)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(inbox)
	if err != nil {
		return nil, err
	}
	var open []Request
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(inbox, entry.Name()))
		if err != nil {
			continue
		}
		var item Request
		if err := json.Unmarshal(data, &item); err != nil {
			continue
		}
		if item.Status == "new" && item.To == project {
			open = append(open, item)
		}
	}
	return open, nil
}
