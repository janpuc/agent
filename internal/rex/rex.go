// Package rex reads and drives Rex sessions through the rex command line.
package rex

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// terminalFlavor is the Rex block flavor that runs a command in a terminal.
const terminalFlavor = "com.superlogical.terminal.shell"

// Session is one Rex session.
type Session struct {
	ID    string `json:"session_id"`
	Label string `json:"label"`
}

// Window is one Rex window. A project's pi tab is the unlabelled window that runs the runner.
type Window struct {
	ID     string  `json:"window_id"`
	Label  string  `json:"label"`
	Layers []Layer `json:"layers"`
}

// Layer is one layer of a window.
type Layer struct {
	Blocks []Block `json:"blocks"`
}

// Block is one block of a layer.
type Block struct {
	ID string `json:"block_id"`
}

// Process is the process state of a block.
type Process struct {
	Child      *ProcessInfo `json:"child"`
	Foreground *ProcessInfo `json:"foreground"`
}

// ProcessInfo names one process.
type ProcessInfo struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
}

// Client is one connected Rex application.
type Client struct {
	ID    string     `json:"client_id"`
	State string     `json:"connection_state"`
	Info  ClientInfo `json:"info"`
}

// ClientInfo says what kind of Rex client this is.
type ClientInfo struct {
	Kind string `json:"kind"`
}

// Created is the result of creating a session or a window.
type Created struct {
	SessionID string
	WindowID  string
}

// call runs one rex command and returns its standard output.
func call(args ...string) ([]byte, error) {
	command := exec.Command("rex", args...)
	command.Stderr = os.Stderr
	return command.Output()
}

// perform runs one rex command whose output is not read.
func perform(args ...string) error {
	command := exec.Command("rex", args...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	return command.Run()
}

// payload renders a rex api call body.
func payload(value any) (string, error) {
	data, err := json.Marshal(value)
	return string(data), err
}

// Sessions lists the sessions of the running Rex server.
func Sessions() ([]Session, error) {
	output, err := call("ls", "--json")
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

// Inspect lists a session's windows.
func Inspect(session string) ([]Window, error) {
	output, err := call("session", "inspect", session, "--json")
	if err != nil {
		return nil, err
	}
	var reply struct {
		Windows []Window `json:"windows"`
	}
	if err := json.Unmarshal(output, &reply); err != nil {
		return nil, fmt.Errorf("invalid rex session inspection: %w", err)
	}
	return reply.Windows, nil
}

// CreateSession creates a session with a pi tab and a shell tab.
func CreateSession(label, folder string, command []string) (Created, error) {
	options := map[string]any{"cwd": folder, "shell": "none", "command": command,
		"exit": map[string]any{"on_completion": false}}
	pi := map[string]any{"layout": map[string]any{"block": map[string]any{
		"flavor": terminalFlavor, "options": options}}}
	shell := map[string]any{"window_label": "shell", "layout": map[string]any{"block": map[string]any{
		"flavor": terminalFlavor, "options": map[string]any{"cwd": folder}}}}
	body, err := payload(map[string]any{
		"label": label, "initial_windows": []any{pi, shell}})
	if err != nil {
		return Created{}, err
	}
	output, err := call("api", "call", "session.create", body)
	if err != nil {
		return Created{}, err
	}
	var reply struct {
		SessionID string `json:"session_id"`
		Windows   []struct {
			WindowID string `json:"window_id"`
		} `json:"initial_windows"`
	}
	if err := json.Unmarshal(output, &reply); err != nil {
		return Created{}, fmt.Errorf("invalid rex session.create reply: %w", err)
	}
	if reply.SessionID == "" || len(reply.Windows) == 0 {
		return Created{}, fmt.Errorf("invalid rex session.create reply: %s", output)
	}
	return Created{SessionID: reply.SessionID, WindowID: reply.Windows[0].WindowID}, nil
}

// NewWindow creates a labelled window in a session and runs command in it. A background window
// leaves the focus where it is.
func NewWindow(session, label, folder string, command []string, background bool) (string, error) {
	args := []string{"window", "new", "-s", session, label, "-c", folder}
	if background {
		args = append(args, "--focus=false")
	}
	args = append(args, "--json")
	output, err := call(append(args, command...)...)
	if err != nil {
		return "", err
	}
	var reply struct {
		WindowID string `json:"window_id"`
	}
	if err := json.Unmarshal(output, &reply); err != nil {
		return "", fmt.Errorf("invalid rex window.new reply: %w", err)
	}
	if reply.WindowID == "" {
		return "", fmt.Errorf("invalid rex window.new reply: %s", output)
	}
	return reply.WindowID, nil
}

// SetWindowLabel renames a window.
func SetWindowLabel(session, window, label string) error {
	body, err := payload(map[string]any{"session_id": session, "window_id": window, "label": label})
	if err != nil {
		return err
	}
	_, err = call("api", "call", "session.set_window_label", body)
	return err
}

// ProcessOf reads a block's process state.
func ProcessOf(session, block string) (*Process, error) {
	output, err := call("block", "call", "-s", session, "-b", block, "process")
	if err != nil {
		return nil, err
	}
	var process Process
	if err := json.Unmarshal(output, &process); err != nil {
		return nil, fmt.Errorf("invalid rex block process reply: %w", err)
	}
	return &process, nil
}

// RunBlock runs command in a new block split off block, in the same window.
func RunBlock(session, block, folder string, command []string) error {
	args := []string{"run", "-s", session, "-b", block, "--split=right", "-c", folder, "--json"}
	_, err := call(append(args, command...)...)
	return err
}

// CloseBlock closes a block.
func CloseBlock(session, block string) error {
	return perform("block", "close", "-s", session, block)
}

// MoveSession moves a session before another one.
func MoveSession(session, before string) error {
	body, err := payload(map[string]any{"session_id": session, "before_session_id": before})
	if err != nil {
		return err
	}
	_, err = call("api", "call", "session.move", body)
	return err
}

// Clients lists the connected Rex applications.
func Clients() ([]Client, error) {
	output, err := call("client", "ls", "--json")
	if err != nil {
		return nil, err
	}
	var reply struct {
		Clients []Client `json:"clients"`
	}
	if err := json.Unmarshal(output, &reply); err != nil {
		return nil, fmt.Errorf("invalid rex client list: %w", err)
	}
	return reply.Clients, nil
}

// Select shows a session and window in a connected Rex application.
func Select(client, session, window string) error {
	return perform("-C", client, "do", "session.select", "session_id="+session, "window_id="+window)
}

// Unique returns the one session labelled label, nil when there is none.
func Unique(sessions []Session, label string) (*Session, error) {
	return uniqueBy(sessions, label, func(session Session) string { return session.Label })
}

// UniqueWindow returns the one window labelled label, nil when there is none.
func UniqueWindow(windows []Window, label string) (*Window, error) {
	return uniqueBy(windows, label, func(window Window) string { return window.Label })
}

// uniqueBy finds the only item with label, refusing an ambiguous set.
func uniqueBy[T any](items []T, label string, labelOf func(T) string) (*T, error) {
	var found *T
	for i := range items {
		if labelOf(items[i]) != label {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple rex resources labelled %s, resolve them first", label)
		}
		found = &items[i]
	}
	return found, nil
}
