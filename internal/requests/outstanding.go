package requests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// OverdueAge is how long a request may wait for its answer before the needs-Jan list marks it
// overdue.
const OverdueAge = 24 * time.Hour

// Waiting is one inbox record that waits on the operator: an unanswered request that has waited
// past OverdueAge, or a reply that answered with OutcomeNeedsJan.
type Waiting struct {
	// Project is the project that owes the answer, so the one the operator has to unblock. It is
	// the owner of an unanswered request and the asker of a needs-jan reply.
	Project string
	// Text is the request or reply text.
	Text string
	// Created is when the record was written.
	Created time.Time
	// Request is the six hex digits that address the request the chain hangs on.
	Request string
	// Chain names who waits on whom, for example "kubernetes waits on jan".
	Chain string
	// Overdue marks an unanswered request older than OverdueAge.
	Overdue bool
}

// record is one inbox file: a request plus the kind that marks a task fold or report, which is
// never a question for the operator.
type record struct {
	Request
	Kind string `json:"kind"`
}

// Outstanding returns what the projects' inboxes say waits on the operator, oldest first. A
// request is open until another record names it in inReplyTo, so an answered item stays out.
// Requests younger than OverdueAge wait a day before they appear. Unreadable records are skipped,
// as a broken file must never hide the rest.
func Outstanding(root string, projects []string, operator string, now time.Time) []Waiting {
	var records []record
	for _, project := range projects {
		box := filepath.Join(root, project, ".agent", "inbox")
		entries, err := os.ReadDir(box)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(box, entry.Name()))
			if err != nil {
				continue
			}
			var item record
			if json.Unmarshal(data, &item) != nil || item.ID == "" {
				continue
			}
			records = append(records, item)
		}
	}
	answered := map[string]bool{}
	for _, item := range records {
		if item.InReplyTo != "" {
			answered[item.InReplyTo] = true
		}
	}
	var waiting []Waiting
	for _, item := range records {
		created, err := time.Parse(time.RFC3339, item.Created)
		if err != nil {
			continue
		}
		switch {
		case item.InReplyTo == "" && item.Kind == "" && item.From != operator &&
			// Replies to work travel by the remote inbox, so they never show up here and would look open forever.
			item.From != "work" &&
			(item.Status == "new" || item.Status == "accepted") && !answered[item.ID]:
			if now.Sub(created) <= OverdueAge {
				continue
			}
			waiting = append(waiting, Waiting{
				Project: item.To, Text: item.Text, Created: created, Request: shortID(item.ID),
				Chain: fmt.Sprintf("%s waits on %s", item.From, item.To), Overdue: true,
			})
		case item.InReplyTo != "" && item.Outcome == OutcomeNeedsJan && !answered[item.ID]:
			waiting = append(waiting, Waiting{
				Project: item.From, Text: item.Text, Created: created, Request: shortID(item.InReplyTo),
				Chain: fmt.Sprintf("%s waits on %s", item.From, operator),
			})
		}
	}
	sort.SliceStable(waiting, func(i, j int) bool {
		a, b := waiting[i], waiting[j]
		if !a.Created.Equal(b.Created) {
			return a.Created.Before(b.Created)
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Text < b.Text
	})
	return waiting
}

// shortID is the six hex digits at the end of a request id, short enough for a phone and unique
// within a project in practice.
func shortID(id string) string {
	if index := strings.LastIndex(id, "-"); index >= 0 && len(id)-index-1 == 6 {
		return id[index+1:]
	}
	return id
}
