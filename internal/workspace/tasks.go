package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// TaskStatuses lists the statuses in a workspace's task state, in name order.
func TaskStatuses(folder string) ([]string, error) {
	state := filepath.Join(folder, ".agent", "tasks")
	entries, err := os.ReadDir(state)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var statuses []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		data, err := os.ReadFile(filepath.Join(state, name, "meta.json"))
		if err != nil {
			return nil, err
		}
		var meta struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		valid := json.Unmarshal(data, &meta) == nil && meta.ID == name &&
			(meta.Status == "open" || meta.Status == "closing" || meta.Status == "closed")
		if info, err := os.Stat(filepath.Join(folder, "tasks", name)); err == nil && info.IsDir() && valid {
			statuses = append(statuses, meta.Status)
			continue
		}
		return nil, fmt.Errorf("%s: invalid task state, restore needed", name)
	}
	return statuses, nil
}
