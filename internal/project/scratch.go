package project

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/requests"
	"github.com/janpuc/agent/internal/workspace"
)

// Scratch runs the scratch commands, which keep free-standing work as tasks of the host's scratch
// project. A task folds into another project or becomes a request to register a new one, and is
// never deleted.
func (h Host) Scratch(scratch string, defaults map[string]int, args []string) error {
	if !h.owns(scratch) {
		return fmt.Errorf("Unknown project: %s", scratch)
	}
	if len(args) == 0 {
		return h.Ensure(scratch)
	}
	if err := requests.Target(h.Registry, h.Home, scratch); err != nil {
		return err
	}
	switch args[0] {
	case "ls":
		if len(args) != 1 {
			return errors.New("Usage: agent scratch ls")
		}
		return h.listProjectTasks(scratch)
	case "new":
		return h.newScratchTask(scratch, args[1:], defaults)
	case "fold":
		if len(args) != 3 {
			return errors.New("Usage: agent scratch fold <id> <project>")
		}
		return h.foldScratchTask(scratch, args[1], args[2])
	case "promote":
		if len(args) != 3 {
			return errors.New("Usage: agent scratch promote <id> <new-project>")
		}
		return h.promoteScratchTask(scratch, args[1], args[2])
	case "restart":
		if len(args) != 2 {
			return errors.New("Usage: agent scratch restart <id>")
		}
		return h.taskAction(scratch, "restart", []string{args[1]}, "")
	}
	return errors.New("Usage: agent scratch [new [--slug slug] <brief>|ls|fold <id> <project>|" +
		"promote <id> <new-project>|restart <id>]")
}

// newScratchTask opens a scratch task with the given slug or a generated one.
func (h Host) newScratchTask(scratch string, rest []string, defaults map[string]int) error {
	var slug, brief string
	switch {
	case len(rest) == 3 && rest[0] == "--slug" && workspace.ValidSlug(rest[1]):
		slug, brief = rest[1], rest[2]
	case len(rest) == 1:
		brief = rest[0]
	default:
		return errors.New("Usage: agent scratch new [--slug slug] <brief>")
	}
	if strings.TrimSpace(brief) == "" {
		return errors.New("A task needs a brief")
	}
	if slug == "" {
		var err error
		if slug, err = h.nextScratchIdentity(scratch); err != nil {
			return err
		}
	}
	return h.newTask(scratch, []string{"--slug", slug, brief}, defaults)
}

// nextScratchIdentity is the next free task id: s-<yymmdd-hhmm> in UTC, then a -2, -3 suffix when
// a task already uses the minute.
func (h Host) nextScratchIdentity(scratch string) (string, error) {
	folder := filepath.Join(h.Root, scratch)
	base := "s-" + time.Now().UTC().Format("060102-1504")
	for n := 1; ; n++ {
		id := base
		if n > 1 {
			id = base + "-" + strconv.Itoa(n)
		}
		target, err := workspace.TaskFolder(folder, id)
		if err != nil {
			return "", err
		}
		if !workspace.Exists(target) && !workspace.Exists(workspace.TaskState(folder, id)) {
			return id, nil
		}
	}
}

// foldScratchTask records the project that takes the task's fold and closes the task, which sends
// the fold to that project once the task session finalizes.
func (h Host) foldScratchTask(scratch, task, project string) error {
	if project == scratch {
		return errors.New("Cannot fold a scratch task into the scratch project")
	}
	if !h.owns(project) {
		return fmt.Errorf("Unknown project: %s", project)
	}
	if err := requests.Target(h.Registry, h.Home, project); err != nil {
		return err
	}
	meta, err := workspace.ReadTask(filepath.Join(h.Root, scratch), task)
	if err != nil {
		return err
	}
	if meta.Status == "closed" {
		return fmt.Errorf("%s: closed", task)
	}
	return h.taskAction(scratch, "close", []string{task}, project)
}

// promoteScratchTask asks the coordinator to register a new project from the task. It writes one
// request and changes nothing else.
func (h Host) promoteScratchTask(scratch, task, project string) error {
	if !registry.ValidName(project) {
		return fmt.Errorf("Invalid project name: %s", project)
	}
	if _, ok := h.Registry.Projects[project]; ok {
		return fmt.Errorf("%s: already registered", project)
	}
	coordinator, err := h.Registry.Coordinator()
	if err != nil {
		return err
	}
	folder := filepath.Join(h.Root, scratch)
	target, err := workspace.TaskFolder(folder, task)
	if err != nil {
		return err
	}
	if _, err := workspace.ReadTask(folder, task); err != nil {
		return err
	}
	id, err := requests.NewID()
	if err != nil {
		return err
	}
	text := "Register new project " + project + " on " + h.Name + " from scratch task " + task +
		" (" + target + "). Then run agent scratch fold " + task + " " + project +
		" so its agent takes the content in."
	item := requests.Request{ID: id, From: scratch, To: coordinator, Text: text,
		Created: requests.Timestamp(), Status: "new"}
	if err := requests.Store(h.Registry, h.Home, h.Name, h.Root, coordinator, id, item); err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}
