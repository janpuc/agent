// Command agent opens and supervises project agents.
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
	"github.com/janpuc/agent/internal/dispatch"
	"github.com/janpuc/agent/internal/jobs"
	"github.com/janpuc/agent/internal/notes"
	"github.com/janpuc/agent/internal/project"
	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/requests"
	"github.com/janpuc/agent/internal/rex"
	"github.com/janpuc/agent/internal/runner"
	"github.com/janpuc/agent/internal/watch"
	"github.com/janpuc/agent/internal/workspace"
)

// helpText is the command guide. It names no site; the registry does.
const helpText = `agent: project agents, dispatched over SSH from a client

Projects
  ls                    List projects and status                 agent ls
  <project> | open       Open or start a project                  agent open home
  up                    Start every active project               agent up
  restart <project>|all Reload running agents on their sessions  agent restart all
  adopt <project>       Record an existing workspace             agent adopt home
  archive <project>     Stop a project and retain all files      agent archive home

Requests
  ask <project> <text>   Ask the system's owner                   agent ask dotfiles "Update the standards"
  reply <project> <id> <text>  Reply from the receiving project    agent reply home <id> "Verified"
  reply <project> <id> <text> --needs-jan  Ask the operator          agent reply kubernetes <id> "Needs a key" --needs-jan

Jobs on workers, machines that are on only when Jan wants (a worker that is off is normal: the job waits)
  job run <worker> <repo> [--commit <rev>] [--fetch <path>]... -- ./<script> [args]  agent job run hermes caption-slate --fetch build -- ./scripts/build.sh
  job ls                List jobs and status                     agent job ls
  job cancel <id>       Cancel a queued job                      agent job cancel <id>

Overview
  todo                  What waits on the operator, oldest first   agent todo
  todo --all            Every STATE.md question and waiting job     agent todo --all
  watch                 Start dead runners, continue stalled turns  agent watch --dry-run
  digest [hours]        Commits, pushes, merges, deploys, overrides  agent digest 48

Tasks
  task new [project] <TICKET|--slug slug> <brief>  Open a task      agent task new home --slug audit "Audit speakers"
  task new [project] --wait <TICKET|--slug slug>  Open, paste brief  agent task new home --wait HA-124
  task new [project] --model <provider/id[:level]> ...  Other model  agent task new home --model antigravity/gemini-4-neon:high --slug try "Try it"
  <TICKET>              Show its task, or open it with --wait       agent HA-125
  task ls [project]     List tasks and status                    agent task ls
  task send [project] <id> <message>  Message a task session  agent task send home audit "Check the bridge too"
  task send --steer [project] <id> <message>  Reach a busy task now  agent task send --steer home audit "Stop, wrong file"
  task ticket [project] <slug> <TICKET>  Attach a ticket            agent task ticket home audit HA-123
  task close [project] <id>    Request a result and knowledge fold agent task close home audit
  task restart [project] <id>  Resume the same task session        agent task restart home audit

Scratch
  scratch [new] [--slug slug] <brief>  Open free-standing work  agent scratch --slug tidy "Tidy notes"
  scratch ls            List the scratch project's tasks          agent scratch ls
  scratch fold <id> <project>  Fold a scratch task into a project agent scratch fold tidy home
  scratch promote <id> <new-project>  Ask for a new project      agent scratch promote tidy garden
  scratch restart <id>  Resume the same scratch task session    agent scratch restart tidy

Inside a project, task commands may omit the project. Close never deletes files.
Pi commands: /rotate checkpoints a project, /inbox lists requests, /reload reloads Pi.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		var status exitStatus
		if errors.As(err, &status) {
			os.Exit(status.code)
		}
		fmt.Fprintf(os.Stderr, "agent: %v\n", err)
		if usageError(err) {
			fmt.Fprintln(os.Stderr, "Run agent --help for all commands.")
		}
		os.Exit(1)
	}
}

// usageError reports whether err is a usage error. Its message is a command's Usage line, so the
// command prints the one line and a pointer to the guide, not the whole guide.
func usageError(err error) bool {
	message := err.Error()
	return strings.HasPrefix(message, "Usage: agent ") || strings.HasPrefix(message, "usage: agent ")
}

// exitStatus carries a child's exit code that the command reports without a message.
type exitStatus struct{ code int }

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func run(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if helpRequest(args) {
		fmt.Print(helpText)
		return nil
	}
	// A launch-open without its project is a usage error like any other.
	if len(args) >= 2 && args[0] == "--launch" && args[1] == "open" && len(args) != 3 {
		return errors.New("Usage: agent open <registered-project>")
	}
	if len(args) == 1 && args[0] == "--worker-endpoint" {
		// A worker such as a laptop is a dispatch client, so this runs before the client check.
		hostPath(home)
		err := jobs.Endpoint(home, os.Stdin, os.Stdout, os.Stderr)
		if code, ok := jobs.ExitCode(err); ok {
			return exitStatus{code}
		}
		return err
	}
	if len(args) == 1 && args[0] == "--main-endpoint" {
		// Before the registry read, so a broken registry can still be repaired by chezmoi update.
		return mainEndpoint(home)
	}
	reg, err := registry.Load(home)
	if err != nil {
		return err
	}
	host, err := registry.HostName()
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "namespace" {
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
	}
	if reg.IsClient(host) {
		if isScratchEntry(args) {
			hub, err := reg.HubSite()
			if err != nil {
				return err
			}
			command, _ := projectCommand(args)
			return clientScratch(hub, command)
		}
		return dispatch.Run(reg, home, host, args)
	}
	if _, ok := reg.Hosts[host]; !ok {
		return fmt.Errorf("Host %s is not registered as an execution host", host)
	}
	if len(args) > 0 && (args[0] == "--client-operator" || args[0] == "--client-jan") {
		return clientRequest(home, args)
	}
	switch {
	case len(args) == 1 && args[0] == "--remote-inbox":
		return requestEntry(home, args, false)
	case len(args) == 1 && args[0] == "--fetch-replies":
		return requestEntry(home, args, false)
	case len(args) == 1 && args[0] == "--digest-daily":
		return digestDaily(home, reg, host)
	case len(args) == 3 && args[0] == "--task-run":
		return taskRun(home, reg, host, args[1], args[2])
	case len(args) == 2 && args[0] == "--run":
		return projectRun(home, reg, host, args[1])
	case len(args) > 0 && args[0] == "--register":
		return registerEntry(home, args)
	}
	command := args
	if len(command) > 0 && command[0] == "--launch" {
		command = command[1:]
	}
	if len(command) > 0 {
		switch command[0] {
		case "task":
			if len(command) == 1 || command[1] == "-h" || command[1] == "--help" {
				fmt.Print(helpText)
				return nil
			}
			return taskEntry(home, command)
		case "scratch":
			return scratchEntry(home, command)
		case "job":
			root, err := reg.Root(home, host)
			if err != nil {
				return err
			}
			return jobs.Entry(reg, home, host, root, command[1:])
		case "ask", "reply":
			return requestEntry(home, command, false)
		case "todo":
			if len(command) > 2 || len(command) == 2 && command[1] != "--all" {
				return errors.New("usage: agent todo [--all]")
			}
			return todo(home, len(command) == 2)
		case "ls", "digest":
			return overview(home, command)
		case "watch":
			return watchEntry(home, command)
		}
		if len(command) == 1 && workspace.ValidTicket(command[0]) {
			return ticketEntry(home, command[0])
		}
		if isProjectEntry(command) {
			return projectEntry(home, command)
		}
	}
	return errors.New("Usage: agent-run <registered-project>")
}

// helpRequest reports the forms that print the command guide instead of running a command.
func helpRequest(args []string) bool {
	if len(args) == 0 {
		return true
	}
	forms := [][]string{
		{"-h"}, {"--help"}, {"help"},
		{"--launch"}, {"--launch", "help"}, {"--launch", "-h"}, {"--launch", "--help"},
		{"--launch", "task"}, {"--launch", "task", "-h"}, {"--launch", "task", "--help"},
		{"task"}, {"task", "-h"}, {"task", "--help"},
	}
	for _, form := range forms {
		if len(form) != len(args) {
			continue
		}
		equal := true
		for i := range form {
			if form[i] != args[i] {
				equal = false
				break
			}
		}
		if equal {
			return true
		}
	}
	return false
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
	return requestEntry(home, rest, true)
}

// requestEntry runs the request commands on this host.
func requestEntry(home string, args []string, clientOperator bool) error {
	reg, err := registry.Load(home)
	if err != nil {
		return err
	}
	host, err := registry.HostName()
	if err != nil {
		return err
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
		return errors.New("Usage: agent reply <project> <id> <text> [--needs-jan]")
	}
	// --needs-jan marks a reply that cannot complete without the operator. It is the last word,
	// so it is never part of the text it follows.
	words, outcome := args[3:], requests.OutcomeDone
	if len(words) > 0 && words[len(words)-1] == "--needs-jan" {
		words, outcome = words[:len(words)-1], requests.OutcomeNeedsJan
	}
	project := args[1]
	if _, ok := reg.Projects[project]; !ok || reg.ProjectHost(project) != host {
		return errors.New("Replies must originate on this project's host")
	}
	return requests.Reply(reg, home, host, root, project, args[2], strings.Join(words, " "), outcome)
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
// adopt, archive or restart. Help and task commands are handled elsewhere.
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
	case "task", "help", "ls", "todo", "digest", "ask", "reply", "namespace", "watch":
		return false
	}
	return !strings.HasPrefix(command[0], "-")
}

// taskEntry runs the task commands on this host.
func taskEntry(home string, args []string) error {
	reg, err := registry.Load(home)
	if err != nil {
		return err
	}
	host, err := registry.HostName()
	if err != nil {
		return err
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
	hostWork := project.Host{Registry: reg, Home: home, Name: host, Root: root}
	return hostWork.Tasks(reg.Defaults.Tasks, command[1:])
}

// ticketEntry brings up or opens the task for a bare ticket id on this host.
func ticketEntry(home, id string) error {
	reg, err := registry.Load(home)
	if err != nil {
		return err
	}
	host, err := registry.HostName()
	if err != nil {
		return err
	}
	hostPath(home)
	if err := validateProjects(reg, home, host); err != nil {
		return err
	}
	root, err := reg.Root(home, host)
	if err != nil {
		return err
	}
	hostWork := project.Host{Registry: reg, Home: home, Name: host, Root: root}
	return hostWork.Ticket(reg.Defaults.Tasks, id)
}

// isScratchEntry reports whether args name a scratch command.
func isScratchEntry(args []string) bool {
	command, _ := projectCommand(args)
	return len(command) > 0 && command[0] == "scratch"
}

// scratchEntry runs the scratch commands on this host against the host's scratch project.
func scratchEntry(home string, args []string) error {
	reg, err := registry.Load(home)
	if err != nil {
		return err
	}
	host, err := registry.HostName()
	if err != nil {
		return err
	}
	command, _ := projectCommand(args)
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
	scratch, err := reg.ScratchProject(host)
	if err != nil {
		return err
	}
	hostWork := project.Host{Registry: reg, Home: home, Name: host, Root: root}
	return hostWork.Scratch(scratch, reg.Defaults.Tasks, command[1:])
}

// clientScratch runs the scratch command on the hub, where the scratch project lives. It is sent
// as one quoted command line for the hub's login shell, without the client's project identity.
func clientScratch(hub string, command []string) error {
	words := make([]string, len(command)+1)
	words[0] = "agent"
	for i, word := range command {
		words[i+1] = quoteWord(word)
	}
	remote := "env -u PI_PROJECT fish -lc " + quoteWord(strings.Join(words, " "))
	cmd := exec.Command("ssh", hub, remote)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// quoteWord quotes one word for a POSIX or fish shell as shlex.quote does.
func quoteWord(word string) string {
	if word != "" && strings.Trim(word, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// projectEntry runs the workspace lifecycle commands on this host.
func projectEntry(home string, args []string) error {
	reg, err := registry.Load(home)
	if err != nil {
		return err
	}
	host, err := registry.HostName()
	if err != nil {
		return err
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
	if command[0] == "up" {
		return hostWork.Up()
	}
	if !onHost(reg, host, command[0]) {
		return fmt.Errorf("Unknown project: %s", command[0])
	}
	return hostWork.Ensure(command[0])
}

// watchEntry runs the one pass watchdog outside Rex. It prints what it does, and with --dry-run it
// changes nothing.
func watchEntry(home string, args []string) error {
	reg, err := registry.Load(home)
	if err != nil {
		return err
	}
	host, err := registry.HostName()
	if err != nil {
		return err
	}
	hostPath(home)
	if _, ok := reg.Hosts[host]; !ok {
		return fmt.Errorf("Host %s is not registered as an execution host", host)
	}
	if err := validateProjects(reg, home, host); err != nil {
		return err
	}
	dryRun := false
	switch {
	case len(args) == 1:
	case len(args) == 2 && args[1] == "--dry-run":
		dryRun = true
	default:
		return errors.New("usage: agent watch [--dry-run]")
	}
	root, err := reg.Root(home, host)
	if err != nil {
		return err
	}
	return watch.Once(project.Host{Registry: reg, Home: home, Name: host, Root: root}, dryRun, os.Stdout)
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
	"scratch": true, "watch": true,
}

// validateProjects refuses project names that shadow a command and broken lifecycles before a
// request command runs.
func validateProjects(reg *registry.Registry, home, host string) error {
	operator, err := reg.Operator()
	if err != nil {
		return err
	}
	for _, name := range reg.Names(host) {
		// A project may be called scratch only when it is its host's scratch project, so agent
		// scratch and agent <project> open the same thing.
		shadows := reservedNames[name] && !(name == "scratch" && reg.Hosts[host].ScratchProject == name)
		if !registry.ValidName(name) || shadows || name == operator {
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

// todo prints the needs-Jan list: every question in a project's STATE.md, unanswered requests
// past their day and replies that asked for the operator, oldest first. With --all it prints the
// flat list of questions and waiting jobs instead.
func todo(home string, all bool) error {
	reg, host, err := local(home)
	if err != nil {
		return err
	}
	root, err := reg.Root(home, host)
	if err != nil {
		return err
	}
	operator, err := reg.Operator()
	if err != nil {
		return err
	}
	var names []string
	for _, name := range reg.Names(host) {
		lifecycle, err := reg.Lifecycle(home, name)
		if err != nil {
			return err
		}
		if lifecycle == "active" {
			names = append(names, name)
		}
	}
	if all {
		for _, name := range names {
			for _, question := range notes.PendingApprovals(filepath.Join(root, name)) {
				fmt.Printf("%s: %s\n", name, question)
			}
		}
		for _, line := range jobs.Waiting(home) {
			fmt.Printf("jobs: %s\n", line)
		}
		return nil
	}
	now := time.Now()
	var items []todoItem
	for _, name := range names {
		// STATE.md dates every question in it, as notes.StateMtime explains.
		written, ok := notes.StateMtime(filepath.Join(root, name))
		if !ok {
			continue
		}
		for _, question := range notes.PendingApprovals(filepath.Join(root, name)) {
			items = append(items, todoItem{created: written, project: name, text: question})
		}
	}
	for _, waiting := range requests.Outstanding(root, names, operator, now) {
		items = append(items, todoItem{created: waiting.Created, project: waiting.Project,
			text: waiting.Text, chain: waiting.Chain, request: waiting.Request, overdue: waiting.Overdue})
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if !a.created.Equal(b.created) {
			return a.created.Before(b.created)
		}
		if a.project != b.project {
			return a.project < b.project
		}
		return a.text < b.text
	})
	for _, item := range items {
		line := fmt.Sprintf("%-4s %s: %s", age(now, item.created), item.project, shorten(item.text, todoText))
		var marks []string
		if item.overdue {
			marks = append(marks, "overdue")
		}
		if item.chain != "" {
			marks = append(marks, item.chain, "request "+item.request)
		}
		if len(marks) > 0 {
			line += " (" + strings.Join(marks, ", ") + ")"
		}
		fmt.Println(line)
	}
	return nil
}

// todoItem is one line of the needs-Jan list. chain, request and overdue are set for an item that
// came from an inbox record.
type todoItem struct {
	created time.Time
	project string
	text    string
	chain   string
	request string
	overdue bool
}

// todoText is how much of an item's text one line carries, so the line still fits a phone.
const todoText = 80

// age dates an item in minutes, hours or days, whichever keeps it short.
func age(now, created time.Time) string {
	since := now.Sub(created)
	switch {
	case since < 0:
		return "0m"
	case since < time.Hour:
		return fmt.Sprintf("%dm", int(since.Minutes()))
	case since < 48*time.Hour:
		return fmt.Sprintf("%dh", int(since.Hours()))
	}
	return fmt.Sprintf("%dd", int(since.Hours()/24))
}

// shorten folds text to one line and cuts it at a word boundary near limit, so a long question
// still reads as a sentence on a phone.
func shorten(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len([]rune(text)) <= limit {
		return text
	}
	runes := []rune(text)
	cut := string(runes[:limit])
	if index := strings.LastIndex(cut, " "); index > limit/2 {
		return cut[:index]
	}
	return cut
}

// overview runs ls and digest, which report on this host's projects.
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
		return fmt.Errorf("Host %s is not registered as an execution host", host)
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

// projectRun is the generation loop a project's pi tab starts. It refuses a project that is
// archived or not initialized yet before starting Pi.
func projectRun(home string, reg *registry.Registry, host, name string) error {
	if _, ok := reg.Projects[name]; !ok || reg.ProjectHost(name) != host {
		return errors.New("Usage: agent-run <registered-project>")
	}
	if err := validateProjects(reg, home, host); err != nil {
		return err
	}
	lifecycle, err := reg.Lifecycle(home, name)
	if err != nil {
		return err
	}
	if lifecycle == "archived" {
		return fmt.Errorf("%s: archived, not starting", name)
	}
	root, err := reg.Root(home, host)
	if err != nil {
		return err
	}
	record, err := workspace.Initialization(home, root, name)
	if err != nil {
		return err
	}
	if record == nil || record.State != "initialized" || !workspace.IsDir(filepath.Join(root, name)) {
		return fmt.Errorf("%s: initialization or restore needed, use agent %s", name, name)
	}
	hostPath(home)
	code, err := runner.Loop(runner.Host{Registry: reg, Home: home, Name: host, Root: root}, name, "")
	if err != nil {
		return err
	}
	return exitStatus{code}
}

// taskRun is the generation loop a task's own tab starts. The task's metadata and its closed
// marker decide whether Pi starts at all.
func taskRun(home string, reg *registry.Registry, host, name, task string) error {
	if _, ok := reg.Projects[name]; !ok || reg.ProjectHost(name) != host {
		return fmt.Errorf("Unknown project: %s", name)
	}
	if err := validateProjects(reg, home, host); err != nil {
		return err
	}
	if err := requests.Target(reg, home, name); err != nil {
		return err
	}
	root, err := reg.Root(home, host)
	if err != nil {
		return err
	}
	hostPath(home)
	code, err := runner.Loop(runner.Host{Registry: reg, Home: home, Name: host, Root: root}, name, task)
	if err != nil {
		return err
	}
	return exitStatus{code}
}

// digestDaily writes today's digest to the state directory and prints its path.
func digestDaily(home string, reg *registry.Registry, host string) error {
	root, err := reg.Root(home, host)
	if err != nil {
		return err
	}
	text, err := digest.Render(digest.Source{
		Root:     root,
		Host:     host,
		Projects: reg.Names(host),
		Log:      filepath.Join(registry.StateDir(home), "actions.jsonl"),
		Hours:    24,
		Now:      time.Now(),
	})
	if err != nil {
		return err
	}
	dir := filepath.Join(registry.StateDir(home), "digest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, time.Now().Format("2006-01-02")+".md")
	if err := workspace.AtomicWrite(path, []byte(text)); err != nil {
		return err
	}
	fmt.Println(path)
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
			providerStatus(reg.Projects[name].Providers.Value), tasks)
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
