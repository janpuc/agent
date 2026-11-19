// Command agent opens and supervises project agents. Commands not ported to Go yet are handed
// to the legacy runner.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/janpuc/agent/internal/legacy"
	"github.com/janpuc/agent/internal/notes"
	"github.com/janpuc/agent/internal/registry"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agent: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	switch {
	case len(args) > 0 && args[0] == "todo":
		if len(args) != 1 {
			return errors.New("usage: agent todo")
		}
		return todo(home)
	case len(args) > 0 && args[0] == "namespace":
		if len(args) > 2 {
			return errors.New("usage: agent namespace [directory]")
		}
		dir := "."
		if len(args) == 2 {
			dir = args[1]
		}
		ns, err := namespace(home, dir)
		if err != nil {
			return err
		}
		fmt.Println(ns)
		return nil
	case len(args) > 1 && (args[0] == "--client-operator" || args[0] == "--client-jan") && args[1] != "--launch":
		// Laptop dispatch marks an operator request ahead of an ordinary command.
		return legacy.Exec(home, append([]string{args[0], "--launch"}, args[1:]...))
	case len(args) == 2 && args[0] == "--run":
		return legacy.Exec(home, args[1:])
	case len(args) > 0 && strings.HasPrefix(args[0], "--"):
		// Entry points for SSH forced commands, Rex tabs and laptop dispatch.
		return legacy.Exec(home, args)
	}
	return legacy.Exec(home, append([]string{"--launch"}, args...))
}

// local loads the registry and checks that this machine is one of its execution hosts.
func local(home string) (*registry.Registry, string, error) {
	reg, err := registry.Load(home)
	if err != nil {
		return nil, "", err
	}
	host, err := registry.HostName()
	if err != nil {
		return nil, "", err
	}
	if _, ok := reg.Hosts[host]; !ok {
		return nil, "", fmt.Errorf("host %s is not registered as an execution host", host)
	}
	return reg, host, nil
}

// todo prints the open questions for the operator from every active project's STATE.md.
func todo(home string) error {
	reg, host, err := local(home)
	if err != nil {
		return err
	}
	root, err := reg.Root(home, host)
	if err != nil {
		return err
	}
	for _, name := range reg.Names(host) {
		lifecycle, err := reg.Lifecycle(home, name)
		if err != nil {
			return err
		}
		if lifecycle != "active" {
			continue
		}
		for _, question := range notes.PendingApprovals(filepath.Join(root, name)) {
			fmt.Printf("%s: %s\n", name, question)
		}
	}
	return nil
}

// namespace selects the memini namespace for a directory. It never derives a name from a path:
// a registered project gets its namespace and everything else its host's scratch namespace. An
// execution host considers only its own root, so it never selects another host's namespace.
// A client considers every host's root.
func namespace(home, dir string) (string, error) {
	reg, err := registry.Load(home)
	if err != nil {
		return "", err
	}
	host, err := registry.HostName()
	if err != nil {
		return "", err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	hosts := []string{host}
	fallback := host
	if _, ok := reg.Hosts[host]; !ok {
		hosts = hosts[:0]
		for name := range reg.Hosts {
			hosts = append(hosts, name)
		}
		sort.Strings(hosts)
		fallback = reg.Platform.Hub
		if fallback == "" {
			fallback = reg.Defaults.Host
		}
	}
	for _, h := range hosts {
		root, err := reg.Root(home, h)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		project, _, _ := strings.Cut(rel, string(filepath.Separator))
		if _, ok := reg.Projects[project]; ok && project != "." && reg.ProjectHost(project) == h {
			return reg.Namespace(project), nil
		}
		return reg.Scratch(h), nil
	}
	return reg.Scratch(fallback), nil
}
