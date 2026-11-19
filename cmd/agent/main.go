// Command agent opens and supervises project agents. Commands not ported to Go yet are handed
// to the legacy runner.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/janpuc/agent/internal/digest"
	"github.com/janpuc/agent/internal/legacy"
	"github.com/janpuc/agent/internal/notes"
	"github.com/janpuc/agent/internal/project"
	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/requests"
	"github.com/janpuc/agent/internal/rex"
	"github.com/janpuc/agent/internal/workspace"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var status exitStatus
		if errors.As(err, &status) {
			os.Exit(status.code)
		}
		fmt.Fprintf(os.Stderr, "agent: %v\n", err)
		os.Exit(1)
	}
}

// exitStatus carries a child's exit code that the command reports without a message.
type exitStatus struct{ code int }

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func run(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	switch {
	case len(args) == 1 && args[0] == "--main-endpoint":
		// Before the registry read, so a broken registry can still be repaired by chezmoi update.
		return mainEndpoint(home)
	case len(args) > 0 && args[0] == "todo":
		if len(args) != 1 {
			return errors.New("usage: agent todo")
		}
		return todo(home)
	case len(args) > 0 && (args[0] == "ls" || args[0] == "digest"):
		return overview(home, args)
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
	case len(args) > 0 && (args[0] == "--client-operator" || args[0] == "--client-jan"):
		return clientRequest(home, args)
	case len(args) == 1 && (args[0] == "--remote-inbox" || args[0] == "--fetch-replies"):
		return requestEntry(home, args, false, args)
	case len(args) > 0 && (args[0] == "ask" || args[0] == "reply"):
		return requestEntry(home, args, false, launchArgs(args))
	case len(args) > 2 && args[0] == "--launch" && (args[1] == "ask" || args[1] == "reply"):
		return requestEntry(home, args[1:], false, args)
	case len(args) == 2 && args[0] == "--run":
		return legacy.Exec(home, args[1:])
	case len(args) > 0 && args[0] == "--register":
		return registerEntry(home, args)
	case isProjectEntry(args):
		return projectEntry(home, args)
	case len(args) > 0 && strings.HasPrefix(args[0], "--"):
		// Entry points for SSH forced commands, Rex tabs and laptop dispatch.
		return legacy.Exec(home, args)
	}
	return legacy.Exec(home, append([]string{"--launch"}, args...))
}

// mainEndpoint is the coordinator's forced SSH command. It accepts exactly three operations and
// runs before the registry is read, as authorized_keys is the gate, not the host name.
func mainEndpoint(home string) error {
	hostPath(home)
	operation := os.Getenv("SSH_ORIGINAL_COMMAND")
	switch operation {
	case "agent ls", "agent restart all", "chezmoi update":
	default:
		return errors.New("main endpoint accepts only agent ls, agent restart all or chezmoi update")
	}
	if err := os.Chdir(home); err != nil {
		return err
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	os.Stdin = devnull
	if operation == "agent ls" {
		return overview(home, []string{"ls"})
	}
	if operation == "agent restart all" {
		return projectEntry(home, []string{"restart", "all"})
	}
	// No TTY: chezmoi scripts that need sudo fail and wait for the operator's local apply.
	command := exec.Command("chezmoi", "--no-tty", "update")
	command.Stdin = devnull
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exitStatus{exit.ExitCode()}
		}
		return err
	}
	return nil
}

// hostPath puts mise shims, Homebrew and the Rex server helpers first on a macOS execution host,
// where SSH and launchd sessions start without them. Clients are never passed here.
func hostPath(home string) {
	if runtime.GOOS != "darwin" {
		return
	}
	os.Setenv("PATH", strings.Join([]string{
		filepath.Join(home, ".local/share/mise/shims"), "/opt/homebrew/bin",
		"/Applications/Rex Beta.app/Contents/Helpers/Rex Server.rex/Contents/MacOS",
		os.Getenv("PATH")}, string(os.PathListSeparator)))
}

// clientRequest accepts an operator ask from a laptop dispatcher and nothing else.
func clientRequest(home string, args []string) error {
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--launch" {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != "ask" {
		return errors.New("--client-operator is only for laptop requests")
	}
	return requestEntry(home, rest, true, launchArgs(args))
}

// launchArgs is the shape older commands hand the legacy runner: a launch flag added to a bare
// command or to an operator flag that came without one.
func launchArgs(args []string) []string {
	if len(args) > 1 && (args[0] == "--client-operator" || args[0] == "--client-jan") && args[1] != "--launch" {
		return append([]string{args[0], "--launch"}, args[1:]...)
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return append([]string{"--launch"}, args...)
	}
	return args
}

// requestEntry runs the request commands. A dispatch-only client, or a host the registry does not
// list, still hands the command to the legacy runner.
func requestEntry(home string, args []string, clientOperator bool, fallback []string) error {
	reg, err := registry.Load(home)
	if err != nil {
		return err
	}
	host, err := registry.HostName()
	if err != nil {
		return err
	}
	if reg.IsClient(host) {
		return legacy.Exec(home, fallback)
	}
	if _, ok := reg.Hosts[host]; !ok {
		return legacy.Exec(home, fallback)
	}
	root, err := reg.Root(home, host)
	if err != nil {
		return err
	}
	switch args[0] {
	case "--remote-inbox":
		return requests.RemoteInbox(reg, home, host, root, os.Stdin)
	case "--fetch-replies":
		return requests.FetchReplies(reg, home, host, root)
	}
	if err := validateProjects(reg, home, host); err != nil {
		return err
	}
	if args[0] == "ask" {
		if len(args) < 3 {
			return errors.New("Usage: agent ask <project> <text>")
		}
		return requests.Ask(reg, home, host, root, args[1], strings.Join(args[2:], " "), clientOperator)
	}
	if len(args) < 4 {
		return errors.New("Usage: agent reply <project> <id> <text>")
	}
	project := args[1]
	if _, ok := reg.Projects[project]; !ok || reg.ProjectHost(project) != host {
		return errors.New("Replies must originate on this project's host")
	}
	return requests.Reply(reg, home, host, root, project, args[2], strings.Join(args[3:], " "))
}

// projectCommand strips one --launch prefix from the arguments of a workspace command. The
// laptop dispatcher sends the prefixed form, a person at the host types the bare one.
func projectCommand(args []string) ([]string, bool) {
	if len(args) > 0 && args[0] == "--launch" {
		return args[1:], true
	}
	return args, false
}

// isProjectEntry reports whether args name a project lifecycle command: a project, open, up,
// adopt, archive or restart. Help and task commands stay with the legacy runner.
func isProjectEntry(args []string) bool {
	command, _ := projectCommand(args)
	if len(command) == 0 {
		return false
	}
	switch command[0] {
	case "up", "adopt", "archive", "restart":
		return true
	case "open":
		return len(command) == 2
	case "task", "help", "ls", "todo", "digest", "ask", "reply", "namespace":
		return false
	}
	return !strings.HasPrefix(command[0], "-")
}

// projectEntry runs the workspace lifecycle commands on this host. A dispatch-only client still
// hands them to the legacy runner's SSH fan-out.
func projectEntry(home string, args []string) error {
	reg, err := registry.Load(home)
	if err != nil {
		return err
	}
	host, err := registry.HostName()
	if err != nil {
		return err
	}
	if reg.IsClient(host) {
		return legacy.Exec(home, launchArgs(args))
	}
	hostPath(home)
	if _, ok := reg.Hosts[host]; !ok {
		return fmt.Errorf("Host %s is not registered as an execution host", host)
	}
	if err := validateProjects(reg, home, host); err != nil {
		return err
	}
	root, err := reg.Root(home, host)
	if err != nil {
		return err
	}
	command, _ := projectCommand(args)
	if len(command) > 1 && command[0] == "open" {
		command = command[1:]
	}
	hostWork := project.Host{Registry: reg, Home: home, Name: host, Root: root}
	if command[0] == "adopt" || command[0] == "archive" {
		if len(command) != 2 || !onHost(reg, host, command[1]) {
			return fmt.Errorf("Usage: agent %s <registered-project>", command[0])
		}
		if command[0] == "archive" {
			return hostWork.Archive(command[1])
		}
		return hostWork.Adopt(command[1])
	}
	if command[0] == "restart" {
		if len(command) != 2 {
			return errors.New("Usage: agent restart <project>|all")
		}
		return hostWork.Restart(command[1])
	}
	if len(command) != 1 {
		return errors.New("Usage: agent <project>|up|ls|archive <project>|adopt <project>|restart <project>|all")
	}
	names := []string{command[0]}
	if command[0] == "up" {
		names = nil
		for _, name := range reg.Names(host) {
			lifecycle, err := reg.Lifecycle(home, name)
			if err != nil {
				return err
			}
			if lifecycle == "active" {
				names = append(names, name)
			}
		}
	}
	for _, name := range names {
		if !onHost(reg, host, name) {
			return fmt.Errorf("Unknown project: %s", name)
		}
		if err := hostWork.Ensure(name); err != nil {
			return err
		}
	}
	return nil
}

// registerEntry adds a member to an initialized workspace for the operator.
func registerEntry(home string, args []string) error {
	reg, err := registry.Load(home)
	if err != nil {
		return err
	}
	host, err := registry.HostName()
	if err != nil {
		return err
	}
	if reg.IsClient(host) {
		return legacy.Exec(home, args)
	}
	hostPath(home)
	if _, ok := reg.Hosts[host]; !ok {
		return fmt.Errorf("Host %s is not registered as an execution host", host)
	}
	if err := validateProjects(reg, home, host); err != nil {
		return err
	}
	if len(args) != 3 && len(args) != 4 {
		return errors.New("Usage: agent-run <registered-project>")
	}
	if !onHost(reg, host, args[1]) {
		return fmt.Errorf("Unknown project: %s", args[1])
	}
	root, err := reg.Root(home, host)
	if err != nil {
		return err
	}
	url := ""
	if len(args) == 4 {
		url = args[3]
	}
	hostWork := project.Host{Registry: reg, Home: home, Name: host, Root: root}
	return hostWork.Register(args[1], args[2], url)
}

// onHost reports whether name is a project this execution host runs.
func onHost(reg *registry.Registry, host, name string) bool {
	_, ok := reg.Projects[name]
	return ok && reg.ProjectHost(name) == host
}

// reservedNames are project names that would shadow a command.
var reservedNames = map[string]bool{
	"up": true, "ls": true, "archive": true, "adopt": true, "ask": true, "reply": true,
	"restart": true, "task": true, "open": true, "help": true, "todo": true, "digest": true,
}

// validateProjects refuses project names that shadow a command and broken lifecycles before a
// request command runs.
func validateProjects(reg *registry.Registry, home, host string) error {
	operator, err := reg.Operator()
	if err != nil {
		return err
	}
	for _, name := range reg.Names(host) {
		if !registry.ValidName(name) || reservedNames[name] || name == operator {
			return errors.New("Project names must be single path components and not reserved command names or the operator")
		}
		if _, err := reg.Lifecycle(home, name); err != nil {
			return err
		}
	}
	return nil
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

// overview runs ls and digest, which report on this host's projects. A dispatch-only client still
// needs the legacy runner's SSH fan-out.
func overview(home string, args []string) error {
	reg, err := registry.Load(home)
	if err != nil {
		return err
	}
	host, err := registry.HostName()
	if err != nil {
		return err
	}
	if _, ok := reg.Hosts[host]; !ok {
		return legacy.Exec(home, append([]string{"--launch"}, args...))
	}
	root, err := reg.Root(home, host)
	if err != nil {
		return err
	}
	// A broken lifecycle refuses every overview command before it prints anything.
	for _, name := range reg.Names(host) {
		if _, err := reg.Lifecycle(home, name); err != nil {
			return err
		}
	}
	if args[0] == "ls" {
		if len(args) != 1 {
			return errors.New("usage: agent ls")
		}
		return list(home, reg, host, root)
	}
	hours := 24
	if len(args) > 2 {
		return errors.New("usage: agent digest [hours]")
	}
	if len(args) == 2 {
		value, err := strconv.Atoi(args[1])
		if err != nil || strings.TrimLeft(args[1], "0123456789") != "" {
			return errors.New("usage: agent digest [hours]")
		}
		hours = value
	}
	text, err := digest.Render(digest.Source{
		Root:     root,
		Host:     host,
		Projects: reg.Names(host),
		Log:      filepath.Join(registry.StateDir(home), "actions.jsonl"),
		Hours:    hours,
		Now:      time.Now(),
	})
	if err != nil {
		return err
	}
	fmt.Print(text)
	return nil
}

// list prints one line per project on this host, then the status of its repositories.
func list(home string, reg *registry.Registry, host, root string) error {
	sessions, err := rex.Sessions()
	if err != nil {
		return err
	}
	names := reg.Names(host)
	for _, name := range names {
		folder := filepath.Join(root, name)
		lifecycle, err := reg.Lifecycle(home, name)
		if err != nil {
			return err
		}
		record, err := workspace.Initialization(home, root, name)
		if err != nil {
			return err
		}
		info, err := os.Stat(folder)
		isDir := err == nil && info.IsDir()
		init := "new"
		switch {
		case record != nil && isDir:
			init = record.State
		case record != nil:
			init = "restore-needed"
		case err == nil:
			init = "needs-adopt"
		}
		session, err := rex.Unique(sessions, name)
		if err != nil {
			return err
		}
		generation, err := workspace.Generation(folder)
		if err != nil {
			return err
		}
		inbox, err := requests.Open(filepath.Join(folder, ".agent", "inbox"), name)
		if err != nil {
			return err
		}
		restart := ""
		if _, err := os.Stat(filepath.Join(folder, ".agent", "restart-requested")); err == nil {
			restart = " restart=pending"
		}
		taskStatuses, err := workspace.TaskStatuses(folder)
		if err != nil {
			return err
		}
		tasks := 0
		for _, status := range taskStatuses {
			if status != "closed" {
				tasks++
			}
		}
		rexID := "no"
		if session != nil {
			rexID = session.ID
		}
		fmt.Printf("%s lifecycle=%s init=%s folder=%s rex=%s pi=%s generation=%d requests=%d%s "+
			"host=%s providers=%s tasks=%d\n", name, lifecycle, init, yesNo(isDir), rexID,
			yesNo(workspace.Running(folder)), generation, len(inbox), restart, host,
			providerStatus(reg.Projects[name].Providers), tasks)
		if !isDir {
			continue
		}
		statuses, err := workspace.RepositoryStatus(folder)
		if err != nil {
			return err
		}
		for _, status := range statuses {
			fmt.Printf("  %s\n", status)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	registered := map[string]bool{}
	for _, name := range names {
		registered[name] = true
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if registered[entry.Name()] {
			continue
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			continue
		}
		fmt.Printf("%s unregistered path=%s\n", entry.Name(), path)
	}
	return nil
}

// yesNo renders a flag the way the listing shows it.
func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// providerStatus is the project's providers as the listing shows them.
func providerStatus(providers any) string {
	list, ok := providers.([]any)
	if !ok {
		if providers == nil {
			return ""
		}
		return fmt.Sprint(providers)
	}
	names := make([]string, len(list))
	for i, provider := range list {
		names[i] = fmt.Sprint(provider)
	}
	return strings.Join(names, ",")
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
