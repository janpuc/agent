package requests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/workspace"
)

// delivered is a request as the hub stores a remote delivery: the remote fields first, then the
// time the hub received it.
type delivered struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Text      string `json:"text"`
	Status    string `json:"status"`
	Created   string `json:"created"`
	InReplyTo string `json:"inReplyTo,omitempty"`
}

// Call runs one operation of the hub's forced command over SSH from the remote host and returns
// its JSON reply.
func Call(reg *registry.Registry, home, host, operation string, payload any) ([]byte, error) {
	remote, err := reg.RemoteSite()
	if err != nil {
		return nil, err
	}
	if host != remote.Host {
		return nil, fmt.Errorf("Only %s uses the remote inbox key", remote.Host)
	}
	hub, err := reg.HubSite()
	if err != nil {
		return nil, err
	}
	input, err := encode(payload, false, true)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none", "-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "StrictHostKeyChecking=yes", "-i", expandHome(home, remote.Identity), hub, operation)
	command.Stdin = bytes.NewReader(input)
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	var reply json.RawMessage
	if err := json.Unmarshal(output, &reply); err != nil {
		return nil, err
	}
	return output, nil
}

// expandHome expands a leading ~ in an identity path.
func expandHome(home, path string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// RemoteInbox is the hub's forced command: it delivers one request or fetches the replies queued
// for the remote project.
func RemoteInbox(reg *registry.Registry, home, host, root string, stdin io.Reader) error {
	hub, err := reg.HubSite()
	if err != nil {
		return err
	}
	remote, err := reg.RemoteSite()
	if err != nil {
		return err
	}
	operation := os.Getenv("SSH_ORIGINAL_COMMAND")
	if host != hub || (operation != "deliver" && operation != "fetch") {
		return fmt.Errorf("Remote inbox accepts only deliver or fetch on %s", hub)
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, Limit+1))
	if err != nil {
		return err
	}
	if len(raw) > Limit {
		return fmt.Errorf("Remote inbox payload exceeds 32 KiB")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("Invalid remote inbox payload: %w", err)
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("Remote inbox requires an object")
	}
	if operation == "deliver" {
		return deliver(reg, home, host, root, hub, remote, payload)
	}
	return fetch(reg, home, host, root, remote, payload)
}

// deliver stores one request from the remote project in a hub inbox after checking every field.
func deliver(reg *registry.Registry, home, host, root, hub string, remote registry.Remote,
	payload map[string]any) error {
	target, _ := payload["to"].(string)
	from, _ := payload["from"].(string)
	id, _ := payload["id"].(string)
	text, textOK := payload["text"].(string)
	status, _ := payload["status"].(string)
	registered := false
	if _, ok := reg.Projects[target]; ok {
		registered = reg.ProjectHost(target) == hub
	}
	if from != remote.Project || !registry.ValidName(target) || !registered || !ValidID(id) ||
		!textOK || strings.TrimSpace(text) == "" || status != "new" {
		return fmt.Errorf("Invalid remote request")
	}
	if err := Target(reg, home, target); err != nil {
		return err
	}
	record, err := workspace.Initialization(home, root, target)
	if err != nil {
		return err
	}
	if record == nil || record.State != "initialized" || !isDirectory(filepath.Join(root, target)) {
		return fmt.Errorf("Target workspace is not initialized")
	}
	item := delivered{ID: id, From: from, To: target, Text: text, Status: status, Created: Timestamp()}
	if value, ok := payload["inReplyTo"]; ok {
		reply, ok := value.(string)
		if !ok || !ValidID(reply) {
			return fmt.Errorf("Invalid reply id")
		}
		item.InReplyTo = reply
	}
	path := filepath.Join(root, target, ".agent/inbox", item.ID+".json")
	if _, err := os.Stat(path); err == nil {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var old map[string]any
		if err := json.Unmarshal(data, &old); err != nil {
			return err
		}
		for _, key := range []string{"from", "to", "text"} {
			if old[key] != payload[key] {
				return fmt.Errorf("Request id already exists")
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if err := write(reg, home, host, root, target, item.ID, item); err != nil {
		return err
	}
	output, err := encode(map[string]string{"delivered": item.ID}, false, true)
	if err != nil {
		return err
	}
	fmt.Println(string(output))
	return nil
}

// fetch removes the acknowledged replies from the work reply queue and prints the rest.
func fetch(reg *registry.Registry, home, host, root string, remote registry.Remote,
	payload map[string]any) error {
	acknowledgements := []string{}
	value, present := payload["ack"]
	for key := range payload {
		if key != "ack" {
			return fmt.Errorf("Invalid work reply acknowledgements")
		}
	}
	if present {
		list, ok := value.([]any)
		if !ok || len(list) > 128 {
			return fmt.Errorf("Invalid work reply acknowledgements")
		}
		for _, item := range list {
			id, ok := item.(string)
			if !ok || !ValidID(id) {
				return fmt.Errorf("Invalid work reply acknowledgements")
			}
			acknowledgements = append(acknowledgements, id)
		}
	}
	queue, err := WorkReplyQueue(home, remote)
	if err != nil {
		return err
	}
	for _, id := range acknowledgements {
		if err := os.Remove(filepath.Join(queue, id+".json")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	entries, err := os.ReadDir(queue)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	items := []Request{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := entry.Info()
		if entry.Type()&os.ModeSymlink != 0 || err != nil || !info.Mode().IsRegular() ||
			!ValidID(strings.TrimSuffix(name, ".json")) {
			return fmt.Errorf("Invalid work reply queue entry")
		}
		raw, err := readLimited(filepath.Join(queue, name))
		if err != nil {
			return err
		}
		if len(raw) > Limit {
			return fmt.Errorf("Work reply exceeds 32 KiB")
		}
		var item Request
		if err := json.Unmarshal(raw, &item); err != nil {
			return fmt.Errorf("Invalid work reply")
		}
		if item.To != remote.Project || item.ID != strings.TrimSuffix(name, ".json") ||
			!ValidID(item.InReplyTo) {
			return fmt.Errorf("Invalid work reply")
		}
		candidate := append(items, item)
		if size, err := encode(candidate, false, true); err != nil {
			return err
		} else if len(size) > Limit {
			break
		}
		items = candidate
	}
	output, err := encode(items, false, true)
	if err != nil {
		return err
	}
	fmt.Println(string(output))
	return nil
}

// FetchReplies takes the replies the hub queued for the remote project, stores the new ones and
// acknowledges all of them.
func FetchReplies(reg *registry.Registry, home, host, root string) error {
	remote, err := reg.RemoteSite()
	if err != nil {
		return err
	}
	if host != remote.Host || os.Getenv("PI_PROJECT") != remote.Project {
		return fmt.Errorf("Only the %s project may fetch replies", remote.Project)
	}
	acknowledgements := filepath.Join(registry.StateDir(home), "work-reply-acks.json")
	pending := []string{}
	if data, err := os.ReadFile(acknowledgements); err == nil {
		if err := json.Unmarshal(data, &pending); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	output, err := Call(reg, home, host, "fetch", map[string]any{"ack": pending})
	if err != nil {
		return err
	}
	var items []json.RawMessage
	if err := json.Unmarshal(output, &items); err != nil {
		return err
	}
	received := []string{}
	for _, raw := range items {
		var value any
		var item Request
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		fields, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("Invalid work reply")
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return fmt.Errorf("Invalid work reply")
		}
		if _, ok := fields["text"].(string); !ok || item.To != remote.Project || !ValidID(item.ID) {
			return fmt.Errorf("Invalid work reply")
		}
		path := filepath.Join(root, item.To, ".agent/inbox", item.ID+".json")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := write(reg, home, host, root, item.To, item.ID, item); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		received = append(received, item.ID)
	}
	if err := os.MkdirAll(filepath.Dir(acknowledgements), 0o755); err != nil {
		return err
	}
	data, err := encode(received, false, true)
	if err != nil {
		return err
	}
	return atomicWrite(acknowledgements, append(data, '\n'))
}

// readLimited reads at most Limit+1 bytes of a reply file.
func readLimited(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, Limit+1))
}
