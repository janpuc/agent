// Package rex reads Rex sessions through the rex command line.
package rex

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// Session is one Rex session.
type Session struct {
	ID    string `json:"session_id"`
	Label string `json:"label"`
}

// Sessions lists the sessions of the running Rex server.
func Sessions() ([]Session, error) {
	output, err := exec.Command("rex", "ls", "--json").Output()
	if err != nil {
		return nil, err
	}
	var reply struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(output, &reply); err != nil {
		return nil, fmt.Errorf("invalid rex session list: %w", err)
	}
	return reply.Sessions, nil
}

// Unique returns the one session labelled label, nil when there is none.
func Unique(sessions []Session, label string) (*Session, error) {
	var found *Session
	for i := range sessions {
		if sessions[i].Label != label {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple rex resources labelled %s, resolve them first", label)
		}
		found = &sessions[i]
	}
	return found, nil
}
