package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/rex"
	"github.com/janpuc/agent/internal/workspace"
)

// stamp is the request time a restart marker carries.
func stamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

// Adopt records an existing workspace whose history predates the agent. It only inspects it.
func (h Host) Adopt(name string) error {
	lock, err := workspace.ProjectLock(h.Home, name)
	if err != nil {
		return err
	}
	defer lock.Close()
	record, err := workspace.Initialization(h.Home, h.Root, name)
	if err != nil {
		return err
	}
	if record != nil {
		return fmt.Errorf("%s: already has an initialization record", name)
	}
	folder := filepath.Join(h.Root, name)
	if err := workspace.RequireGitRoot(folder); err != nil {
		return err
	}
	dirty, err := workspace.Git(folder, "status", "--porcelain")
	if err != nil {
		return err
	}
	fmt.Printf("%s: workspace dirty=%s\n", name, yesNo(dirty != ""))
	// Adoption deliberately does not migrate ignored legacy members or consume seeds.
	statuses, err := workspace.RepositoryStatus(folder)
	if err != nil {
		return err
	}
	for _, status := range statuses {
		fmt.Printf("%s\n", status)
	}
	return workspace.RecordInitialization(h.Home, h.Root, name, "adopted", "initialized")
}

// Archive stops a project's runner and records the archived lifecycle, keeping every file.
func (h Host) Archive(name string) error {
	lock, err := workspace.ProjectLock(h.Home, name)
	if err != nil {
		return err
	}
	defer lock.Close()
	folder := filepath.Join(h.Root, name)
	tasks, err := workspace.TaskRecords(folder)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.Status != "closed" {
			return fmt.Errorf("%s: close open tasks before archiving", name)
		}
	}
	sessions, err := rex.Sessions()
	if err != nil {
		return err
	}
	session, err := rex.Unique(sessions, name)
	if err != nil {
		return err
	}
	if session != nil {
		if err := h.stopRunner(name, folder, session.ID); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for workspace.Running(folder) {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s: runner did not stop, lifecycle unchanged", name)
		}
		time.Sleep(100 * time.Millisecond)
	}
	path := filepath.Join(registry.StateDir(h.Home), "lifecycle", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return workspace.AtomicWrite(path, []byte("archived\n"))
}

// stopRunner closes the runner's tab, but only when the process in it is provably its runner or
// an idle shell.
func (h Host) stopRunner(name, folder, session string) error {
	window, err := h.piWindow(session, folder)
	if err != nil {
		return err
	}
	if window == nil {
		return nil
	}
	blocks := windowBlocks(window)
	if len(blocks) > 1 {
		return fmt.Errorf("%s: ambiguous pi tab, not archiving", name)
	}
	if len(blocks) == 0 {
		return nil
	}
	block := blocks[0].ID
	process, err := rex.ProcessOf(session, block)
	if err != nil {
		return err
	}
	child := process.Child
	idle := child != nil && shellNames[child.Name] &&
		process.Foreground != nil && process.Foreground.PID == child.PID
	pid, pidErr := workspace.RunnerPID(folder)
	managed := child != nil && workspace.Running(folder) && pidErr == nil && child.PID == pid
	if child != nil && !idle && !managed {
		return fmt.Errorf("%s: live unmanaged process or unknown runner PID, not archiving", name)
	}
	if child != nil && !idle {
		// Keep an exited terminal in this window so closing its runner cannot remove the pi
		// tab. A later launch can replace it.
		if err := rex.RunBlock(session, block, folder, []string{"--shell=none", "--keep-open", "--", "true"}); err != nil {
			return err
		}
		if err := rex.CloseBlock(session, block); err != nil {
			return err
		}
	}
	return nil
}

// Restart asks runners to reload their session. A restart marker is a file the runner consumes
// between Pi starts, so this command only writes it.
func (h Host) Restart(action string) error {
	var targets []string
	if action == "all" {
		for _, name := range h.Registry.Names(h.Name) {
			lifecycle, err := h.Registry.Lifecycle(h.Home, name)
			if err != nil {
				return err
			}
			if lifecycle == "active" && workspace.Running(filepath.Join(h.Root, name)) {
				targets = append(targets, name)
			}
		}
	} else {
		if _, ok := h.Registry.Projects[action]; !ok || h.Registry.ProjectHost(action) != h.Name {
			return fmt.Errorf("Unknown project: %s", action)
		}
		lifecycle, err := h.Registry.Lifecycle(h.Home, action)
		if err != nil {
			return err
		}
		if lifecycle == "archived" {
			return fmt.Errorf("%s: archived, not taking requests", action)
		}
		targets = []string{action}
	}
	for _, name := range targets {
		state := filepath.Join(h.Root, name, ".agent")
		if err := os.MkdirAll(state, 0o755); err != nil {
			return err
		}
		if err := workspace.AtomicWrite(filepath.Join(state, "restart-requested"), []byte(stamp()+"\n")); err != nil {
			return err
		}
	}
	if action == "all" {
		for _, name := range h.Registry.Names(h.Name) {
			lifecycle, err := h.Registry.Lifecycle(h.Home, name)
			if err != nil {
				return err
			}
			if lifecycle != "active" {
				continue
			}
			folder := filepath.Join(h.Root, name)
			tasks, err := workspace.TaskRecords(folder)
			if err != nil {
				return err
			}
			for _, task := range tasks {
				if task.Status == "closed" || !workspace.RunningTask(folder, task.ID) {
					continue
				}
				path := filepath.Join(workspace.TaskState(folder, task.ID), "restart-requested")
				if err := workspace.AtomicWrite(path, []byte(stamp()+"\n")); err != nil {
					return err
				}
			}
		}
	}
	if len(targets) == 0 {
		fmt.Println("restart requested: no running project agents")
		return nil
	}
	fmt.Println("restart requested: " + strings.Join(targets, ", "))
	return nil
}

// Register adds a member repository to an initialized workspace. The caller commits the
// workspace checkpoint that records its gitlink.
func (h Host) Register(name, member, url string) error {
	lock, err := workspace.ProjectLock(h.Home, name)
	if err != nil {
		return err
	}
	defer lock.Close()
	folder := filepath.Join(h.Root, name)
	record, err := workspace.Initialization(h.Home, h.Root, name)
	if err != nil {
		return err
	}
	if record == nil || record.State != "initialized" || !workspace.IsDir(folder) {
		return fmt.Errorf("%s: initialization or restore needed", name)
	}
	return workspace.RegisterRepository(folder, name, member, url)
}

// yesNo renders a flag the way the commands show it.
func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
