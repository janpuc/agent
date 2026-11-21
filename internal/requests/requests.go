// Package requests carries requests and replies between project inboxes.
package requests

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/janpuc/agent/internal/actions"
	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/workspace"
)

// Limit is the largest remote payload, in bytes, the remote inbox accepts.
const Limit = 32768

var requestID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}-[0-9a-f]{6}$`)

// ValidID reports whether id has the request id form.
func ValidID(id string) bool { return requestID.MatchString(id) }

// Request is one request record. An inbox file may carry other fields.
type Request struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Text      string `json:"text"`
	Created   string `json:"created"`
	Status    string `json:"status"`
	InReplyTo string `json:"inReplyTo,omitempty"`
}

// NewID is a new request id: the UTC time and six random hex digits.
func NewID() (string, error) {
	random := make([]byte, 3)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(random), nil
}

// Timestamp is the request time in UTC.
func Timestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

// Target checks that name is a registered project that takes requests.
func Target(reg *registry.Registry, home, name string) error {
	if _, ok := reg.Projects[name]; !ok {
		return fmt.Errorf("Unknown project: %s", name)
	}
	lifecycle, err := reg.Lifecycle(home, name)
	if err != nil {
		return err
	}
	if lifecycle == "archived" {
		return fmt.Errorf("%s: archived, not taking requests", name)
	}
	return nil
}

// WorkReplyQueue is the hub directory that holds replies for the remote project. It must stay
// inside home and have no symlink on the way, as only the remote host reads it.
func WorkReplyQueue(home string, remote registry.Remote) (string, error) {
	state := registry.StateDir(home)
	queue := filepath.Join(state, "remote-inbox")
	if filepath.IsAbs(remote.Project) {
		queue = remote.Project
	} else {
		queue = filepath.Join(queue, remote.Project)
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	if !within(resolved(absolute), resolved(queue)) {
		return "", errors.New("Unsafe work reply queue")
	}
	if info, err := os.Lstat(queue); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Unsafe work reply queue")
	}
	for parent := filepath.Dir(queue); parent != absolute && within(absolute, parent); parent = filepath.Dir(parent) {
		if info, err := os.Lstat(parent); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("Unsafe work reply queue")
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	return queue, nil
}

// within reports whether path is base itself or below it.
func within(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolved follows every existing symlink in path, including one below a missing component, the
// way Path.resolve() does for a path that does not exist yet.
func resolved(path string) string {
	path = filepath.Clean(path)
	if target, err := filepath.EvalSymlinks(path); err == nil {
		return target
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(resolved(parent), filepath.Base(path))
}

// Store writes a request record to target's inbox, or to the work reply queue when the hub
// holds a reply for the remote project.
func Store(reg *registry.Registry, home, host, root, target, id string, item any) error {
	return write(reg, home, host, root, target, id, item)
}

// write puts a request in target's inbox, or in the work reply queue when the hub holds a reply
// for the remote project.
func write(reg *registry.Registry, home, host, root, target, id string, item any) error {
	remote, err := reg.RemoteSite()
	if err != nil {
		return err
	}
	folder := filepath.Join(root, target, ".agent/inbox")
	if target == remote.Project {
		hub, err := reg.HubSite()
		if err != nil {
			return err
		}
		if host == hub {
			if folder, err = WorkReplyQueue(home, remote); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return err
	}
	data, err := encode(item, true, false)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(folder, id+".json"), append(data, '\n'))
}

// atomicWrite writes a temporary file beside path and renames it over path.
func atomicWrite(path string, data []byte) error {
	temp := path + ".tmp"
	defer os.Remove(temp)
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// encode renders a value as Python's json.dumps does: an indented file, or compact JSON with a
// space after each separator. ascii escapes non-ASCII characters, which Python does by default.
func encode(value any, indent, ascii bool) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if indent {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	data := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	if !indent {
		data = separated(data)
	}
	if ascii {
		data = escaped(data)
	}
	return data, nil
}

// separated adds the space after each comma and colon that Python's default separators use.
func separated(data []byte) []byte {
	out := make([]byte, 0, len(data)+16)
	inString, escapedByte := false, false
	for _, b := range data {
		out = append(out, b)
		switch {
		case !inString:
			if b == '"' {
				inString = true
			} else if b == ',' || b == ':' {
				out = append(out, ' ')
			}
		case escapedByte:
			escapedByte = false
		case b == '\\':
			escapedByte = true
		case b == '"':
			inString = false
		}
	}
	return out
}

// escaped replaces non-ASCII characters inside strings with the \u escapes Python writes.
func escaped(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString, escapedByte := false, false
	for i := 0; i < len(data); {
		b := data[i]
		switch {
		case !inString:
			out = append(out, b)
			if b == '"' {
				inString = true
			}
			i++
		case escapedByte:
			out = append(out, b)
			escapedByte = false
			i++
		case b == '\\':
			out = append(out, b)
			escapedByte = true
			i++
		case b == '"':
			out = append(out, b)
			inString = false
			i++
		case b < utf8.RuneSelf:
			out = append(out, b)
			i++
		default:
			r, size := utf8.DecodeRune(data[i:])
			if r > 0xFFFF {
				r -= 0x10000
				out = append(out, fmt.Sprintf("\\u%04x\\u%04x",
					0xD800+(r>>10), 0xDC00+(r&0x3FF))...)
			} else {
				out = append(out, fmt.Sprintf("\\u%04x", r)...)
			}
			i += size
		}
	}
	return out
}

// senderInCwd names the local project whose workspace holds the current directory, so a shell
// outside a Pi session can still ask as that project. It returns "" anywhere else.
func senderInCwd(reg *registry.Registry, host, root string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, cwd)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ""
	}
	name, _, _ := strings.Cut(relative, string(filepath.Separator))
	if _, ok := reg.Projects[name]; !ok || reg.ProjectHost(name) != host {
		return ""
	}
	return name
}

// Ask delivers one request from a project agent, or from the operator's laptop client.
func Ask(reg *registry.Registry, home, host, root, target, text string, clientOperator bool) error {
	if err := Target(reg, home, target); err != nil {
		return err
	}
	sender := os.Getenv("PI_PROJECT")
	if sender == "" && !clientOperator {
		sender = senderInCwd(reg, host, root)
	}
	operator, err := reg.Operator()
	if err != nil {
		return err
	}
	remote, err := reg.RemoteSite()
	if err != nil {
		return err
	}
	if clientOperator {
		if sender != "" {
			return fmt.Errorf("A project session cannot send as %s", operator)
		}
		sender = operator
	}
	if sender == "" {
		return errors.New("agent ask needs a sender: run it in a project session or inside a project workspace")
	}
	if sender != operator {
		if _, ok := reg.Projects[sender]; !ok {
			return fmt.Errorf("Unknown sender project: %s", sender)
		}
	}
	if sender == operator && !clientOperator {
		return fmt.Errorf("%s is reserved for the laptop client", operator)
	}
	if sender == target {
		return errors.New("Cannot ask your own project, do the work yourself")
	}
	isRemote := reg.ProjectHost(target) != host
	if isRemote && !(host == remote.Host && sender == remote.Project) {
		hub, err := reg.HubSite()
		if err != nil {
			return err
		}
		return fmt.Errorf("Only %s may deliver remote requests to %s", remote.Project, hub)
	}
	if !isRemote {
		record, err := workspace.Initialization(home, root, target)
		if err != nil {
			return err
		}
		if record == nil || !isDirectory(filepath.Join(root, target)) {
			return fmt.Errorf("%s: workspace is not initialized yet", target)
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("A request needs text")
	}
	id, err := NewID()
	if err != nil {
		return err
	}
	item := Request{ID: id, From: sender, To: target, Text: text, Created: Timestamp(), Status: "new"}
	if isRemote {
		if _, err := Call(reg, home, host, "deliver", item); err != nil {
			return err
		}
	} else if err := write(reg, home, host, root, target, item.ID, item); err != nil {
		return err
	}
	actions.Append(home, host, "a request", fmt.Sprintf("agent ask %s: %s", target, text))
	fmt.Printf("%s: request %s\n", target, item.ID)
	return nil
}

// Reply answers an inbox request from the project that received it.
func Reply(reg *registry.Registry, home, host, root, project, id, text string) error {
	if err := Target(reg, home, project); err != nil {
		return err
	}
	if os.Getenv("PI_PROJECT") != project {
		return errors.New("agent reply must run in the project that received the request")
	}
	if !ValidID(id) {
		return fmt.Errorf("Invalid request id: %s", id)
	}
	var original map[string]any
	data, err := os.ReadFile(filepath.Join(root, project, ".agent/inbox", id+".json"))
	if err != nil || json.Unmarshal(data, &original) != nil ||
		original["id"] != id || original["to"] != project {
		return fmt.Errorf("%s: no request %s in its inbox", project, id)
	}
	target, _ := original["from"].(string)
	operator, err := reg.Operator()
	if err != nil {
		return err
	}
	// The operator has no laptop inbox. Their replies wait in the coordinator's ordinary inbox.
	// Aliases map senders of requests sent before a project rename.
	if target == operator {
		if target, err = reg.Coordinator(); err != nil {
			return err
		}
	}
	if alias, ok := reg.Platform.Aliases[target]; ok {
		target = alias
	}
	if err := Target(reg, home, target); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("A reply needs text")
	}
	newID, err := NewID()
	if err != nil {
		return err
	}
	item := Request{ID: newID, From: project, To: target, Text: text, Created: Timestamp(),
		Status: "new", InReplyTo: id}
	payload, err := encode(item, false, true)
	if err != nil {
		return err
	}
	if len(payload) > Limit-1024 {
		return errors.New("Reply exceeds inbox payload limit")
	}
	remote, err := reg.RemoteSite()
	if err != nil {
		return err
	}
	hub, err := reg.HubSite()
	if err != nil {
		return err
	}
	switch {
	case host == hub && target == remote.Project:
		err = write(reg, home, host, root, target, item.ID, item)
	case reg.ProjectHost(target) != host:
		if host != remote.Host || project != remote.Project {
			return fmt.Errorf("Only %s may deliver replies remotely", remote.Project)
		}
		_, err = Call(reg, home, host, "deliver", item)
	default:
		err = write(reg, home, host, root, target, item.ID, item)
	}
	if err != nil {
		return err
	}
	actions.Append(home, host, "a reply", fmt.Sprintf("agent reply %s %s: %s", project, id, text))
	fmt.Printf("%s: reply %s to request %s\n", target, item.ID, id)
	return nil
}

// isDirectory reports whether path is a directory.
func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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
