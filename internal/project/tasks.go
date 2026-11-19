package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/janpuc/agent/internal/requests"
	"github.com/janpuc/agent/internal/rex"
	"github.com/janpuc/agent/internal/workspace"
)

// resultHeadings are the sections a task's close commit must contain.
var resultHeadings = []string{"Outcome", "Changes", "Verification", "Learned", "Open"}

// foldRequest is the knowledge fold a closed task asks its project to perform.
type foldRequest struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Kind    string `json:"kind"`
	Task    string `json:"task"`
	Text    string `json:"text"`
	Created string `json:"created"`
	Status  string `json:"status"`
}

// Tasks runs the task commands for this execution host. The project argument is optional and
// falls back to PI_PROJECT, as inside a project session.
func (h Host) Tasks(defaults map[string]int, args []string) error {
	if len(args) == 0 {
		return errors.New("Usage: agent task new|ls|ticket|close|restart [project] ...")
	}
	action, rest := args[0], args[1:]
	if action == "ls" && len(rest) == 0 {
		return h.listTasks()
	}
	name := os.Getenv("PI_PROJECT")
	if len(rest) > 0 && h.owns(rest[0]) {
		name, rest = rest[0], rest[1:]
	}
	if err := h.taskTarget(name); err != nil {
		return err
	}
	if action == "ls" {
		if len(rest) > 0 {
			return errors.New("Usage: agent task ls [project]")
		}
		return h.listProjectTasks(name)
	}
	switch action {
	case "new":
		return h.newTask(name, rest, defaults)
	case "ticket", "close", "restart", "finalize":
		passed := 1
		if action == "ticket" {
			passed = 2
		}
		if len(rest) != passed {
			return errors.New("Usage: agent task ticket|close|restart [project] <id> [TICKET]")
		}
		return h.taskAction(name, action, rest, "")
	}
	return errors.New("Usage: agent task ticket|close|restart [project] <id> [TICKET]")
}

// owns reports whether name is a project this execution host runs.
func (h Host) owns(name string) bool {
	_, ok := h.Registry.Projects[name]
	return ok && h.Registry.ProjectHost(name) == h.Name
}

// taskTarget checks that a task command names a project on this host that takes requests.
func (h Host) taskTarget(name string) error {
	if !h.owns(name) {
		return fmt.Errorf("Unknown project: %s", name)
	}
	return requests.Target(h.Registry, h.Home, name)
}

// listTasks lists the tasks of every project on this host.
func (h Host) listTasks() error {
	for _, name := range h.Registry.Names(h.Name) {
		if err := h.listProjectTasks(name); err != nil {
			return err
		}
	}
	return nil
}

// listProjectTasks lists one project's tasks.
func (h Host) listProjectTasks(name string) error {
	folder := filepath.Join(h.Root, name)
	records, err := workspace.TaskRecords(folder)
	if err != nil {
		return err
	}
	for _, meta := range records {
		fmt.Printf("%s %s status=%s ticket=%s pi=%s host=%s\n", name, meta.ID, meta.Status,
			meta.TicketName(), yesNo(workspace.RunningTask(folder, meta.ID)), h.Name)
	}
	return nil
}

// newTask opens a task, starts its runner and never reuses an identity.
func (h Host) newTask(name string, rest []string, defaults map[string]int) error {
	var task, brief, ticket string
	switch {
	case len(rest) == 3 && rest[0] == "--slug" && workspace.ValidSlug(rest[1]):
		task, brief = rest[1], rest[2]
	case len(rest) == 2 && workspace.ValidTicket(rest[0]):
		task, brief = rest[0], rest[1]
		ticket = task
	default:
		return errors.New("Usage: agent task new [project] <TICKET-ID|--slug slug> <brief>")
	}
	if strings.TrimSpace(brief) == "" {
		return errors.New("A task needs a brief")
	}
	if err := h.Ensure(name); err != nil {
		return err
	}
	folder := filepath.Join(h.Root, name)
	lock, err := workspace.ProjectLock(h.Home, name)
	if err != nil {
		return err
	}
	defer lock.Close()
	target, err := workspace.TaskFolder(folder, task)
	if err != nil {
		return err
	}
	state := workspace.TaskState(folder, task)
	if workspace.Exists(target) || workspace.Exists(state) {
		return fmt.Errorf("%s: task identity already used, folders are never reused", task)
	}
	limit := 4
	if value, ok := h.Registry.Projects[name].Tasks["maxOpen"]; ok {
		limit = value
	} else if value, ok := defaults["maxOpen"]; ok {
		limit = value
	}
	records, err := workspace.TaskRecords(folder)
	if err != nil {
		return err
	}
	open := 0
	for _, meta := range records {
		if meta.Status != "closed" {
			open++
		}
	}
	if open >= limit {
		return fmt.Errorf("%s: maxOpen=%d, close an existing task first", name, limit)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		return err
	}
	var attached *string
	if ticket != "" {
		value := ticket
		attached = &value
	}
	meta := &workspace.Task{ID: task, Ticket: attached, Opened: requests.Timestamp(), Status: "open"}
	briefFile := filepath.Join(target, "BRIEF.md")
	if err := os.WriteFile(briefFile,
		[]byte("---\nid: "+task+"\n---\n\n"+strings.TrimSpace(brief)+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(target, "STATE.md"), []byte(workspace.StatePage), 0o644); err != nil {
		return err
	}
	if err := workspace.SaveTask(folder, meta); err != nil {
		return err
	}
	if err := workspace.UpdateBrief(folder, meta); err != nil {
		return err
	}
	if err := h.startTask(name, task); err != nil {
		return err
	}
	fmt.Printf("%s: task %s opened\n", name, task)
	return nil
}

// taskAction runs ticket, close, restart and finalize under the project lock. A non-empty foldTo
// sends the close's fold to that project instead of the task's own one.
func (h Host) taskAction(name, action string, rest []string, foldTo string) error {
	task := rest[0]
	folder := filepath.Join(h.Root, name)
	if action == "close" || action == "restart" {
		meta, err := workspace.ReadTask(folder, task)
		if err != nil {
			return err
		}
		if meta.Status != "closed" {
			if err := h.Ensure(name); err != nil {
				return err
			}
		}
	}
	lock, err := workspace.ProjectLock(h.Home, name)
	if err != nil {
		return err
	}
	defer lock.Close()
	meta, err := workspace.ReadTask(folder, task)
	if err != nil {
		return err
	}
	state := workspace.TaskState(folder, task)
	switch {
	case action == "finalize":
		if os.Getenv("PI_PROJECT") != name || os.Getenv("PI_TASK") != task {
			return errors.New("Only the task session may finalize its close")
		}
		if err := h.finalizeTask(name, task); err != nil {
			return err
		}
	case meta.Status == "closed":
		if action != "close" {
			return fmt.Errorf("%s: closed", task)
		}
		if err := h.finalizeTask(name, task); err != nil {
			return err
		}
	case action == "ticket":
		if !workspace.ValidTicket(rest[1]) {
			return errors.New("Invalid ticket identity")
		}
		if meta.Ticket != nil && *meta.Ticket != "" && *meta.Ticket != rest[1] {
			return errors.New("A ticket is already attached")
		}
		value := rest[1]
		meta.Ticket = &value
		if err := workspace.SaveTask(folder, meta); err != nil {
			return err
		}
		if err := workspace.UpdateBrief(folder, meta); err != nil {
			return err
		}
	case action == "close":
		if foldTo != "" {
			if meta.FoldTo != "" && meta.FoldTo != foldTo {
				return fmt.Errorf("%s: foldTo is already %s", task, meta.FoldTo)
			}
			meta.FoldTo = foldTo
		}
		meta.Status = "closing"
		if meta.FoldRequest == "" {
			if meta.FoldRequest, err = requests.NewID(); err != nil {
				return err
			}
		}
		if err := workspace.SaveTask(folder, meta); err != nil {
			return err
		}
		if err := workspace.UpdateBrief(folder, meta); err != nil {
			return err
		}
		id, err := requests.NewID()
		if err != nil {
			return err
		}
		if err := workspace.AtomicWrite(filepath.Join(state, "close-requested"), []byte(id+"\n")); err != nil {
			return err
		}
		if err := h.startTask(name, task); err != nil {
			return err
		}
	case action == "restart":
		if workspace.RunningTask(folder, task) {
			if err := workspace.AtomicWrite(filepath.Join(state, "restart-requested"),
				[]byte(requests.Timestamp()+"\n")); err != nil {
				return err
			}
		} else if err := h.startTask(name, task); err != nil {
			return err
		}
	}
	fmt.Printf("%s: task %s %s, folder retained\n", name, task, action)
	return nil
}

// startTask launches the legacy runner in the task's own Rex tab. It leaves a live unmanaged
// process and an ambiguous tab alone, and deletes nothing.
func (h Host) startTask(name, task string) error {
	folder := filepath.Join(h.Root, name)
	meta, err := workspace.ReadTask(folder, task)
	if err != nil {
		return err
	}
	if meta.Status == "closed" {
		return fmt.Errorf("%s: closed", task)
	}
	if workspace.RunningTask(folder, task) {
		return nil
	}
	if err := h.requireCapacity(); err != nil {
		return err
	}
	sessions, err := rex.Sessions()
	if err != nil {
		return err
	}
	session, err := rex.Unique(sessions, name)
	if err != nil {
		return err
	}
	if session == nil {
		return fmt.Errorf("%s: start the project first", name)
	}
	target, err := workspace.TaskFolder(folder, task)
	if err != nil {
		return err
	}
	command := taskRunnerCommand(h.Home, name, task)
	windows, err := rex.Inspect(session.ID)
	if err != nil {
		return err
	}
	window, err := rex.UniqueWindow(windows, "task:"+task)
	if err != nil {
		return err
	}
	if window == nil {
		_, err := rex.NewWindow(session.ID, "task:"+task, target, command, true)
		return err
	}
	blocks := windowBlocks(window)
	if len(blocks) != 1 {
		return fmt.Errorf("%s: ambiguous tab, leaving it alone", task)
	}
	process, err := rex.ProcessOf(session.ID, blocks[0].ID)
	if err != nil {
		return err
	}
	if liveUnmanaged(process) {
		return fmt.Errorf("%s: unmanaged process, leaving it alone", task)
	}
	if err := rex.RunBlock(session.ID, blocks[0].ID, target, command); err != nil {
		return err
	}
	return rex.CloseBlock(session.ID, blocks[0].ID)
}

// taskRunnerCommand is the legacy runner launch a task tab uses.
func taskRunnerCommand(home, name, task string) []string {
	words := []string{"python3", filepath.Join(home, ".local/bin/agent-run"), "--task-run", name, task}
	for i, word := range words {
		words[i] = shellQuote(word)
	}
	return []string{"--shell=none", "--keep-open", "--", "fish", "-lc", "exec " + strings.Join(words, " ")}
}

// finalizeTask commits a requested close. It is idempotent, writes the fold request once and
// deletes no task, worktree or session data.
func (h Host) finalizeTask(name, task string) error {
	folder := filepath.Join(h.Root, name)
	meta, err := workspace.ReadTask(folder, task)
	if err != nil {
		return err
	}
	state := workspace.TaskState(folder, task)
	target, err := workspace.TaskFolder(folder, task)
	if err != nil {
		return err
	}
	result := filepath.Join(target, "RESULT.md")
	if meta.Status == "open" || !workspace.Exists(filepath.Join(state, "close-requested")) {
		return errors.New("Close was not requested")
	}
	text, err := os.ReadFile(result)
	if err != nil {
		return err
	}
	for _, heading := range resultHeadings {
		if !regexp.MustCompile(`(?m)^## ` + heading + `\s*$`).Match(text) {
			return errors.New("RESULT.md needs Outcome, Changes, Verification, Learned and Open headings")
		}
	}
	foldTo := name
	foldText := "Fold task " + task + " from " + result + " into notes/PROJECT.md and notes/STATE.md. " +
		"Reconcile learned facts and open threads. Do not assume pending approvals were granted."
	if meta.FoldTo != "" {
		// A scratch task's fold goes to the project that takes its content in, not to the
		// scratch project it was born in.
		foldTo = meta.FoldTo
		foldText = "Take scratch task " + task + " from " + result + ": copy what belongs into your " +
			"workspace, fold RESULT.md into notes/PROJECT.md and notes/STATE.md, and leave the " +
			"scratch folder in place. Do not assume pending approvals were granted."
	}
	inbox := filepath.Join(h.Root, foldTo, ".agent", "inbox")
	if !workspace.Exists(filepath.Join(inbox, meta.FoldRequest+".json")) {
		item := foldRequest{
			ID: meta.FoldRequest, From: name, To: foldTo, Kind: "fold", Task: task,
			Text: foldText, Created: requests.Timestamp(), Status: "new",
		}
		if err := requests.Store(h.Registry, h.Home, h.Name, h.Root, foldTo, item.ID, item); err != nil {
			return err
		}
	}
	meta.Status = "closed"
	if err := workspace.SaveTask(folder, meta); err != nil {
		return err
	}
	if err := workspace.UpdateBrief(folder, meta); err != nil {
		return err
	}
	return workspace.AtomicWrite(filepath.Join(state, "closed"), []byte(requests.Timestamp()+"\n"))
}
