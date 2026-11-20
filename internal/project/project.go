// Package project runs a project's workspace lifecycle on one execution host: bootstrap, member
// restore, capacity, Rex tabs, adopt, archive and restart. Each pi tab it opens runs the Go
// runner's loop.
package project

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/rex"
	"github.com/janpuc/agent/internal/workspace"
)

// Host is this execution host: the registry, its home, its name and workspace root.
type Host struct {
	Registry *registry.Registry
	Home     string
	Name     string
	Root     string
}

// shellNames are the processes an idle tab shows: the shell the runner replaced or the runner's
// own shell command.
var shellNames = map[string]bool{"fish": true, "bash": true, "sh": true, "zsh": true}

// Ensure opens or starts a project: bootstrap, capacity, member restore and its Rex tabs. It
// never changes an existing member or workspace.
func (h Host) Ensure(name string) error {
	lock, err := workspace.ProjectLock(h.Home, name)
	if err != nil {
		return err
	}
	defer lock.Close()
	lifecycle, err := h.Registry.Lifecycle(h.Home, name)
	if err != nil {
		return err
	}
	if lifecycle == "archived" {
		return fmt.Errorf("%s: archived, not starting", name)
	}
	folder, err := workspace.Bootstrap(h.Registry, h.Home, h.Root, name)
	if err != nil {
		return err
	}
	running := workspace.Running(folder)
	if !running {
		if err := h.requireCapacity(); err != nil {
			return err
		}
	}
	members, err := workspace.Members(folder)
	if err != nil {
		return err
	}
	for _, member := range members {
		if err := workspace.RestoreMember(folder, member); err != nil {
			return err
		}
	}
	state := filepath.Join(folder, ".agent")
	if err := os.MkdirAll(state, 0o755); err != nil {
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
	command := runnerCommand(h.Home, name)
	var sid, windowID string
	if session == nil {
		created, err := rex.CreateSession(name, folder, command[3:])
		if err != nil {
			return err
		}
		sid, windowID = created.SessionID, created.WindowID
		if err := writeWindow(state, windowID); err != nil {
			return err
		}
	} else {
		sid = session.ID
		window, err := h.piWindow(sid, folder)
		if err != nil {
			return err
		}
		if window == nil {
			windowID, err = rex.NewWindow(sid, "pi", folder, command, false)
			if err != nil {
				return err
			}
			if err := writeWindow(state, windowID); err != nil {
				return err
			}
			if err := rex.SetWindowLabel(sid, windowID, ""); err != nil {
				return err
			}
		} else {
			windowID = window.ID
			if err := writeWindow(state, windowID); err != nil {
				return err
			}
			if window.Label == "pi" {
				if err := rex.SetWindowLabel(sid, windowID, ""); err != nil {
					return err
				}
			}
		}
		if window != nil && !workspace.Running(folder) {
			blocks := windowBlocks(window)
			if len(blocks) != 1 {
				return fmt.Errorf("%s: pi tab has multiple blocks, leaving it alone", name)
			}
			process, err := rex.ProcessOf(sid, blocks[0].ID)
			if err != nil {
				return err
			}
			if liveUnmanaged(process) {
				return fmt.Errorf("%s: pi tab has a live unmanaged process, leaving it alone", name)
			}
			// Rex has no restart method. Replace only the stopped or idle terminal in the
			// existing window, not the window itself, so the pi tab is never duplicated.
			if err := rex.RunBlock(sid, blocks[0].ID, folder, command); err != nil {
				return err
			}
			if err := rex.CloseBlock(sid, blocks[0].ID); err != nil {
				return err
			}
		}
	}
	windows, err := rex.Inspect(sid)
	if err != nil {
		return err
	}
	shell, err := rex.UniqueWindow(windows, "shell")
	if err != nil {
		return err
	}
	if shell == nil {
		if _, err := rex.NewWindow(sid, "shell", folder, nil, true); err != nil {
			return err
		}
	}
	if err := h.orderSessions(); err != nil {
		return err
	}
	return h.selectSession(name, sid, windowID)
}

// runnerCommand is the launch every pi tab uses.
func runnerCommand(home, name string) []string {
	script := "exec " + shellQuote(filepath.Join(home, ".local/bin/agent")) + " --run " + shellQuote(name)
	return []string{"--shell=none", "--keep-open", "--", fishPath(), "-lc", script}
}

// fishPath is fish by absolute path. The Rex server resolves a tab command with its own PATH,
// which on macOS lacks the Homebrew prefix where fish lives.
func fishPath() string {
	if path, err := exec.LookPath("fish"); err == nil && filepath.IsAbs(path) {
		return path
	}
	return "fish"
}

// shellQuote quotes one word as shlex.quote does for a login shell.
func shellQuote(word string) string {
	if word != "" && strings.Trim(word, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// writeWindow records the pi tab so a later launch replaces that tab and not another one.
func writeWindow(state, window string) error {
	return workspace.AtomicWrite(filepath.Join(state, "rex-window"), []byte(window+"\n"))
}

// piWindow is the tab recorded in the workspace, or the one unlabelled pi tab.
func (h Host) piWindow(session, folder string) (*rex.Window, error) {
	windows, err := rex.Inspect(session)
	if err != nil {
		return nil, err
	}
	marker := filepath.Join(folder, ".agent", "rex-window")
	if info, err := os.Stat(marker); err == nil && info.Mode().IsRegular() {
		if data, err := os.ReadFile(marker); err == nil {
			id := strings.TrimSpace(string(data))
			for i := range windows {
				if windows[i].ID == id {
					return &windows[i], nil
				}
			}
		}
	}
	return rex.UniqueWindow(windows, "pi")
}

// windowBlocks flattens a window's blocks across its layers.
func windowBlocks(window *rex.Window) []rex.Block {
	var blocks []rex.Block
	for _, layer := range window.Layers {
		blocks = append(blocks, layer.Blocks...)
	}
	return blocks
}

// liveUnmanaged reports a process in the tab that the runner did not start and that is still
// running, which no lifecycle command may replace or close.
func liveUnmanaged(process *rex.Process) bool {
	child := process.Child
	if child == nil {
		return false
	}
	idle := shellNames[child.Name] && process.Foreground != nil && process.Foreground.PID == child.PID
	return !idle
}

// orderSessions puts the coordinator first and the rest in label order.
func (h Host) orderSessions() error {
	sessions, err := rex.Sessions()
	if err != nil {
		return err
	}
	coordinator, err := h.Registry.Coordinator()
	if err != nil {
		return err
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		first := sessions[i].Label == coordinator
		second := sessions[j].Label == coordinator
		if first != second {
			return first
		}
		return sessions[i].Label < sessions[j].Label
	})
	for i := len(sessions) - 2; i >= 0; i-- {
		if err := rex.MoveSession(sessions[i].ID, sessions[i+1].ID); err != nil {
			return err
		}
	}
	return nil
}

// selectSession shows the tab in every connected Rex app. Rex cannot say which app the operator is
// looking at, so none is skipped.
func (h Host) selectSession(name, session, window string) error {
	clients, err := rex.Clients()
	if err != nil {
		return err
	}
	var failed error
	for _, client := range clients {
		if client.Info.Kind != "app" || client.State != "connected" {
			continue
		}
		if err := rex.Select(client.ID, session, window); err != nil && failed == nil {
			failed = fmt.Errorf("%s: showing the tab in Rex app %s: %w", name, client.ID, err)
		}
	}
	return failed
}

// requireCapacity refuses a new runner when this host already runs maxAgents of them.
func (h Host) requireCapacity() error {
	limit := h.Registry.Hosts[h.Name].MaxAgents
	if limit == nil {
		return nil
	}
	count := 0
	for _, name := range h.Registry.Names(h.Name) {
		folder := filepath.Join(h.Root, name)
		if workspace.Running(folder) {
			count++
		}
		tasks, err := workspace.TaskRecords(folder)
		if err != nil {
			return err
		}
		for _, task := range tasks {
			if workspace.RunningTask(folder, task.ID) {
				count++
			}
		}
	}
	if count >= *limit {
		return fmt.Errorf("%s: maxAgents=%d, stop an agent first", h.Name, *limit)
	}
	return nil
}
