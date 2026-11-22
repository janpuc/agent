package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// call is one logged stub invocation.
type call struct {
	Tool         string            `json:"tool"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env"`
	Cwd          string            `json:"cwd"`
	Stdin        string            `json:"stdin"`
	StdinDevNull bool              `json:"stdinDevNull"`
	// Generation is the project's .agent/generation when Pi started, empty when missing.
	Generation string `json:"generation"`
}

var stubs = map[string]func() int{
	"rex":     rexStub,
	"pi":      piStub,
	"mise":    miseStub,
	"ssh":     sshStub,
	"chezmoi": func() int { logCall(""); return 0 },
}

func testDir() string { return os.Getenv("AGENT_TEST_DIR") }

func logCall(stdin string) call {
	c := call{Tool: filepath.Base(os.Args[0]), Args: os.Args[1:], Env: map[string]string{}, Stdin: stdin}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		c.Env[key] = value
	}
	c.Cwd, _ = os.Getwd()
	var in, null syscall.Stat_t
	if syscall.Fstat(0, &in) == nil && syscall.Stat(os.DevNull, &null) == nil {
		c.StdinDevNull = in.Rdev == null.Rdev && in.Ino == null.Ino
	}
	if dir := c.Env["PI_PROJECT_DIR"]; dir != "" && c.Tool == "pi" {
		if data, err := os.ReadFile(filepath.Join(dir, ".agent/generation")); err == nil {
			c.Generation = string(data)
		}
	}
	line, _ := json.Marshal(c)
	log, err := os.OpenFile(filepath.Join(testDir(), "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	defer log.Close()
	log.Write(append(line, '\n'))
	return c
}

func readStdin() string {
	data, _ := io.ReadAll(os.Stdin)
	return string(data)
}

// mise env --json prints mise-env.json, other mise commands print nothing.
func miseStub() int {
	logCall("")
	if len(os.Args) > 1 && os.Args[1] == "env" {
		data, err := os.ReadFile(filepath.Join(testDir(), "mise-env.json"))
		if err != nil {
			data = []byte("{}")
		}
		os.Stdout.Write(data)
	}
	return 0
}

// ssh logs its arguments and input and prints ssh-out.json.
// workerStub is the machine behind ssh <name> when worker.json names it: offline, cut off
// during a job, or a real worker endpoint in its own home.
type workerStub struct {
	Name    string `json:"name"`
	Home    string `json:"home"`
	Agent   string `json:"agent"`
	Offline bool   `json:"offline"`
	DropJob bool   `json:"dropJob"`
}

func sshStub() int {
	var worker workerStub
	if data, err := os.ReadFile(filepath.Join(testDir(), "worker.json")); err == nil {
		json.Unmarshal(data, &worker)
	}
	if worker.Name != "" && len(os.Args) > 1 && os.Args[1] == worker.Name {
		logCall("")
		operation := strings.Join(os.Args[2:], " ")
		if worker.Offline || worker.DropJob && operation == "job" {
			io.Copy(io.Discard, os.Stdin)
			fmt.Fprintf(os.Stderr, "ssh: connect to host %s port 22: Operation timed out\n", worker.Name)
			return 255
		}
		command := exec.Command(worker.Agent, "--worker-endpoint")
		command.Env = append(os.Environ(), "HOME="+worker.Home, "SSH_ORIGINAL_COMMAND="+operation)
		command.Dir = worker.Home
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				return exit.ExitCode()
			}
			return 255
		}
		return 0
	}
	logCall(readStdin())
	data, err := os.ReadFile(filepath.Join(testDir(), "ssh-out.json"))
	if err != nil {
		data = []byte("{}")
	}
	os.Stdout.Write(data)
	return 0
}

// piAction is what the Pi stub does on one start: write files, then exit.
type piAction struct {
	Write map[string]string `json:"write"`
	Exit  int               `json:"exit"`
}

// Pi follows pi-script.json, one action per start in order. Later starts exit 0.
func piStub() int {
	logCall("")
	counter := filepath.Join(testDir(), "pi-count")
	count := 0
	if data, err := os.ReadFile(counter); err == nil {
		fmt.Sscan(string(data), &count)
	}
	os.WriteFile(counter, []byte(fmt.Sprint(count+1)), 0o644)
	// A runner that relaunches Pi without end fails the test instead of hanging it.
	if count >= 20 {
		fmt.Fprintln(os.Stderr, "pi stub: more than 20 starts, killing the runner")
		syscall.Kill(os.Getppid(), syscall.SIGKILL)
		return 99
	}
	var script []piAction
	if data, err := os.ReadFile(filepath.Join(testDir(), "pi-script.json")); err == nil {
		if err := json.Unmarshal(data, &script); err != nil {
			panic(err)
		}
	}
	if count >= len(script) {
		return 0
	}
	for path, text := range script[count].Write {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			panic(err)
		}
	}
	return script[count].Exit
}

// rexState is a small model of a Rex server: sessions, windows and blocks.
type rexState struct {
	Sessions []*rexSession `json:"sessions"`
	Clients  []any         `json:"clients"`
	// SelectFails makes every app refuse session.select, like an app that does not answer.
	SelectFails bool `json:"select_fails"`
	// Process answers block call <block> process. Missing blocks report an idle terminal.
	Process map[string]any `json:"process"`
	Next    int            `json:"next"`
}

type rexSession struct {
	ID      string       `json:"session_id"`
	Label   string       `json:"label"`
	Windows []*rexWindow `json:"windows"`
}

type rexWindow struct {
	ID     string     `json:"window_id"`
	Label  string     `json:"label,omitempty"`
	Layers []rexLayer `json:"layers"`
}

type rexLayer struct {
	Blocks []rexBlock `json:"blocks"`
}

type rexBlock struct {
	ID string `json:"block_id"`
}

func rexPath() string { return filepath.Join(testDir(), "rex.json") }

func loadRex() *rexState {
	state := &rexState{Process: map[string]any{}}
	if data, err := os.ReadFile(rexPath()); err == nil {
		if err := json.Unmarshal(data, state); err != nil {
			panic(err)
		}
	}
	return state
}

func (s *rexState) save() {
	data, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(rexPath(), data, 0o644); err != nil {
		panic(err)
	}
}

func (s *rexState) id(kind string) string {
	s.Next++
	return fmt.Sprintf("%s:%d", kind, s.Next)
}

func (s *rexState) session(id string) *rexSession {
	for _, session := range s.Sessions {
		if session.ID == id {
			return session
		}
	}
	panic("rex stub: no session " + id)
}

func (s *rexState) window(label string) *rexWindow {
	return &rexWindow{ID: s.id("window"), Label: label, Layers: []rexLayer{{Blocks: []rexBlock{{ID: s.id("block")}}}}}
}

// flag returns the value after name, as in -s session:1.
func flag(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func rexStub() int {
	logCall("")
	args := os.Args[1:]
	state := loadRex()
	defer state.save()
	reply := func(value any) int {
		json.NewEncoder(os.Stdout).Encode(value)
		return 0
	}
	switch {
	case len(args) >= 1 && args[0] == "-C":
		if state.SelectFails {
			fmt.Fprintln(os.Stderr, `Error: "Hermes" did not answer within 10s; the action may still run.`)
			return 1
		}
		return reply(map[string]any{})
	case len(args) >= 1 && args[0] == "ls":
		sessions := []map[string]any{}
		for _, session := range state.Sessions {
			sessions = append(sessions, map[string]any{"session_id": session.ID, "label": session.Label})
		}
		return reply(map[string]any{"sessions": sessions})
	case len(args) >= 2 && args[0] == "client" && args[1] == "ls":
		if state.Clients == nil {
			state.Clients = []any{}
		}
		return reply(map[string]any{"clients": state.Clients})
	case len(args) >= 3 && args[0] == "session" && args[1] == "inspect":
		return reply(map[string]any{"windows": state.session(args[2]).Windows})
	case len(args) >= 4 && args[0] == "api" && args[1] == "call":
		var body map[string]any
		if err := json.Unmarshal([]byte(args[3]), &body); err != nil {
			panic(err)
		}
		switch args[2] {
		case "session.create":
			session := &rexSession{ID: state.id("session"), Label: body["label"].(string)}
			var initial []map[string]any
			for _, spec := range body["initial_windows"].([]any) {
				label, _ := spec.(map[string]any)["window_label"].(string)
				window := state.window(label)
				session.Windows = append(session.Windows, window)
				initial = append(initial, map[string]any{"window_id": window.ID})
			}
			state.Sessions = append(state.Sessions, session)
			return reply(map[string]any{"session_id": session.ID, "initial_windows": initial})
		case "session.set_window_label":
			for _, window := range state.session(body["session_id"].(string)).Windows {
				if window.ID == body["window_id"] {
					window.Label = body["label"].(string)
				}
			}
			return reply(map[string]any{})
		case "session.move":
			return reply(map[string]any{})
		}
	case len(args) >= 5 && args[0] == "window" && args[1] == "new":
		session := state.session(flag(args, "-s"))
		window := state.window(args[4])
		session.Windows = append(session.Windows, window)
		return reply(map[string]any{"window_id": window.ID})
	case len(args) >= 2 && args[0] == "block" && args[1] == "call":
		if process, ok := state.Process[flag(args, "-b")]; ok {
			return reply(process)
		}
		return reply(map[string]any{"child": nil, "foreground": nil, "last_exit": map[string]any{"exit_code": 0}})
	case len(args) >= 1 && args[0] == "run":
		// --split=right adds a block next to -b in the same window.
		target, added := flag(args, "-b"), state.id("block")
		for _, window := range state.session(flag(args, "-s")).Windows {
			for i, layer := range window.Layers {
				for _, block := range layer.Blocks {
					if block.ID == target {
						window.Layers[i].Blocks = append(window.Layers[i].Blocks, rexBlock{ID: added})
						return reply(map[string]any{"block_ids": []string{added}})
					}
				}
			}
		}
		panic("rex stub: no block " + target)
	case len(args) >= 4 && args[0] == "block" && args[1] == "close":
		for _, window := range state.session(flag(args, "-s")).Windows {
			for i, layer := range window.Layers {
				var kept []rexBlock
				for _, block := range layer.Blocks {
					if block.ID != args[len(args)-1] {
						kept = append(kept, block)
					}
				}
				window.Layers[i].Blocks = kept
			}
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "rex stub: unsupported %q\n", args)
	return 2
}
