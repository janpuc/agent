// Package dispatch sends commands from a dispatch-only client to the host that runs them. The
// client runs no agent commands itself, only quoted SSH command lines.
package dispatch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/workspace"
)

// program is the agent command the remote login shell runs. Every host runs the Go command.
const program = "agent"

// Run sends one agent command to the hosts that run it. A bare command and a --launch command
// are the same; the remote side accepts both.
func Run(reg *registry.Registry, home, client string, args []string) error {
	command := args
	if len(command) > 0 && command[0] == "--launch" {
		command = command[1:]
	}
	if len(command) == 0 {
		return errors.New("The laptop only dispatches agent commands")
	}
	if command[0] == "open" && len(command) == 2 {
		command = command[1:]
	}
	sender := os.Getenv("PI_PROJECT")
	if sender == "" {
		sender = senderInCwd(reg, home)
	}
	if sender != "" {
		if _, ok := reg.Projects[sender]; !ok {
			return fmt.Errorf("Unknown sender project: %s", sender)
		}
	}
	if command[0] == "task" && len(command) >= 2 && command[1] != "ls" {
		if len(command) < 3 || !registered(reg, command[2]) {
			if sender == "" {
				return errors.New("Task commands need an explicit project outside a project")
			}
			command = append(command[:2], append([]string{sender}, command[2:]...)...)
		}
	}
	var hosts []string
	var err error
	if len(command) == 1 && workspace.ValidTicket(command[0]) {
		// A bare ticket runs where the sender project lives, else where tickets open.
		name := sender
		if name == "" {
			name = reg.Platform.Tickets
		}
		if !registered(reg, name) {
			return errors.New("No project takes tickets, use agent task new <project> --wait " + command[0])
		}
		hosts = []string{projectHost(reg, name)}
	} else if hosts, err = targets(reg, command); err != nil {
		return err
	}
	printOutput := command[0] == "digest" || command[0] == "todo" ||
		equal(command, "ls") || equal(command, "task", "ls")
	for _, host := range hosts {
		if _, ok := reg.Hosts[host]; !ok || !registry.ValidName(host) {
			return fmt.Errorf("Unknown host: %s", host)
		}
		remote := []string{"env"}
		if sender != "" {
			remote = append(remote, "PI_PROJECT="+sender)
		} else {
			remote = append(remote, "-u", "PI_PROJECT")
		}
		words := []string{program}
		if command[0] == "ask" && sender == "" {
			// The remote command also accepts the older --client-jan spelling.
			words = append(words, "--client-operator")
		}
		words = append(words, "--launch")
		words = append(words, command...)
		remote = append(remote, "fish", "-lc", join(words))
		if printOutput {
			output, err := sshOutput(host, join(remote))
			if err != nil {
				return err
			}
			text := strings.TrimSuffix(output, "\n")
			if text == "" {
				continue
			}
			for _, line := range strings.Split(text, "\n") {
				fmt.Printf("%s %s\n", host, line)
			}
			continue
		}
		if err := sshRun(host, join(remote)); err != nil {
			return err
		}
	}
	return nil
}

// targets are the hosts that run a command: every host for the host-wide commands, otherwise
// the host the named project runs on.
func targets(reg *registry.Registry, command []string) ([]string, error) {
	hosts := map[string]bool{}
	for name := range reg.Projects {
		hosts[projectHost(reg, name)] = true
	}
	list := make([]string, 0, len(hosts))
	for host := range hosts {
		list = append(list, host)
	}
	hub := reg.Platform.Hub
	if hub == "" {
		hub = reg.Defaults.Host
	}
	sort.Slice(list, func(i, j int) bool {
		if (list[i] != hub) != (list[j] != hub) {
			return list[i] == hub
		}
		return list[i] < list[j]
	})
	if equal(command, "ls") || equal(command, "up") || equal(command, "restart", "all") ||
		equal(command, "task", "ls") || command[0] == "todo" || command[0] == "digest" {
		return list, nil
	}
	index := 0
	if command[0] == "task" {
		index = 2
	} else if command[0] == "ask" || command[0] == "reply" || command[0] == "restart" ||
		command[0] == "adopt" || command[0] == "archive" {
		index = 1
	}
	if len(command) <= index || !registered(reg, command[index]) {
		return nil, errors.New("Use agent ls, <project>, ask, reply, restart all or task with a registered project")
	}
	return []string{projectHost(reg, command[index])}, nil
}

// senderInCwd is the project whose workspace holds the working directory, if any.
func senderInCwd(reg *registry.Registry, home string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	for _, host := range sortedHosts(reg) {
		root, err := reg.Root(home, host)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(root, cwd)
		if err != nil || relative == "." || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		candidate, _, _ := strings.Cut(relative, string(filepath.Separator))
		if registered(reg, candidate) {
			return candidate
		}
	}
	return ""
}

// sortedHosts lists host names in a stable order.
func sortedHosts(reg *registry.Registry) []string {
	names := make([]string, 0, len(reg.Hosts))
	for name := range reg.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// projectHost is the host a project runs on.
func projectHost(reg *registry.Registry, name string) string {
	if host := reg.Projects[name].Host; host != "" {
		return host
	}
	if host := reg.Defaults.Host; host != "" {
		return host
	}
	return reg.Platform.Hub
}

// registered reports whether name is a registered project.
func registered(reg *registry.Registry, name string) bool {
	_, ok := reg.Projects[name]
	return ok
}

// equal reports whether command is exactly words.
func equal(command []string, words ...string) bool {
	if len(command) != len(words) {
		return false
	}
	for i := range command {
		if command[i] != words[i] {
			return false
		}
	}
	return true
}

// sshOutput runs one remote command and returns its standard output.
func sshOutput(host, remote string) (string, error) {
	command := exec.Command("ssh", host, remote)
	command.Stderr = os.Stderr
	output, err := command.Output()
	return string(output), err
}

// sshRun runs one remote command with the client's standard streams.
func sshRun(host, remote string) error {
	command := exec.Command("ssh", host, remote)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

// join quotes a command line for the remote login shell.
func join(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = quote(word)
	}
	return strings.Join(quoted, " ")
}

// quote quotes one word for a POSIX or fish shell as shlex.quote does.
func quote(word string) string {
	if word != "" && strings.Trim(word, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}
