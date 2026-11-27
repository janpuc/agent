// Package tests drives the agent command from outside, through its command line, files and the
// external tools it calls. Every external tool except git is a stub: this test binary, linked
// under the tool's name. AGENT_BIN=/path/to/agent tests a prebuilt command, and without it the
// suite builds ./cmd/agent.
package tests

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

var implementation struct {
	command []string // how to start the agent command
	err     error
}

func TestMain(m *testing.M) {
	if stub, ok := stubs[filepath.Base(os.Args[0])]; ok {
		os.Exit(stub())
	}
	bin := os.Getenv("AGENT_BIN")
	dir, err := os.MkdirTemp("", "agent-build-")
	if err != nil {
		panic(err)
	}
	switch {
	case bin != "":
		implementation.command = []string{bin}
	default:
		out := filepath.Join(dir, "agent")
		build := exec.Command("go", "build", "-o", out, "./cmd/agent")
		build.Dir = ".."
		if output, err := build.CombinedOutput(); err != nil {
			implementation.err = fmt.Errorf("no implementation to test, go build ./cmd/agent: %s", output)
		}
		implementation.command = []string{out}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fixture is one machine: a home directory with a registry, a host name and stubs on PATH.
type fixture struct {
	t        *testing.T
	dir      string // stub state and logs
	home     string
	host     string
	registry map[string]any
	extra    []string // environment for the next command only
	stdin    string
	cwd      string // working directory for the next command, home when empty
}

// The site in every fixture: an operator who sends requests from a laptop, a hub host that
// runs most projects and a remote outpost host whose one project talks to the hub over SSH.
func baseRegistry() map[string]any {
	return map[string]any{
		"platform": map[string]any{
			"operator": "alice", "coordinator": "admin", "aliases": map[string]any{"legacy": "admin"},
			"clients": []any{"laptop"}, "hub": "hub",
			"remote": map[string]any{"project": "field", "host": "outpost", "identity": "~/.ssh/field-inbox"},
		},
		"hosts": map[string]any{
			"hub": map[string]any{"root": "~/Projects", "meminiPrefix": "lab", "meminiHome": "none", "guard": "personal",
				"scratchProject": "workbench"},
			"outpost": map[string]any{"root": "~/Field", "meminiPrefix": "field", "meminiHome": "none", "guard": "work"},
		},
		"defaults": map[string]any{
			"host": "hub", "model": "test/model", "thinking": "high", "providers": "all",
			"meminiPrefix": "lab", "tasks": map[string]any{"maxOpen": 4},
		},
		"projects": map[string]any{
			"admin":     map[string]any{"host": "hub", "lifecycle": "active"},
			"garden":    map[string]any{"host": "hub", "lifecycle": "active"},
			"workbench": map[string]any{"host": "hub", "lifecycle": "active"},
			"field":     map[string]any{"host": "outpost", "lifecycle": "active", "meminiNamespace": "field"},
		},
	}
}

func newFixture(t *testing.T, host string) *fixture {
	t.Helper()
	if implementation.err != nil {
		t.Skip(implementation.err)
	}
	// Resolve the temporary directory, macOS links /var to /private/var.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	f := &fixture{t: t, dir: dir, home: filepath.Join(dir, "home"), host: host, registry: baseRegistry()}
	self, err := os.Executable()
	must(t, err)
	bin := filepath.Join(dir, "bin")
	for _, path := range []string{bin, filepath.Join(f.home, ".pi/agent/local/bin")} {
		must(t, os.MkdirAll(path, 0o755))
	}
	for name := range stubs {
		if name != "pi" {
			must(t, os.Symlink(self, filepath.Join(bin, name)))
		}
	}
	// Pi is started by absolute path, never from PATH.
	must(t, os.Symlink(self, filepath.Join(f.home, ".pi/agent/local/bin/pi")))
	f.saveRegistry()
	return f
}

func (f *fixture) project(name string) map[string]any {
	return f.registry["projects"].(map[string]any)[name].(map[string]any)
}

func (f *fixture) saveRegistry() {
	f.write(".config/agents/projects.json", mustJSON(f.t, f.registry))
}

func (f *fixture) root() string {
	if f.host == "outpost" {
		return filepath.Join(f.home, "Field")
	}
	return filepath.Join(f.home, "Projects")
}

func (f *fixture) workspace(name string) string { return filepath.Join(f.root(), name) }

func (f *fixture) environment() []string {
	env := []string{
		"HOME=" + f.home,
		"PATH=" + filepath.Join(f.dir, "bin") + ":/usr/local/bin:/usr/bin:/bin",
		"AGENT_HOST=" + f.host,
		"AGENT_TEST_DIR=" + f.dir,
		"XDG_CONFIG_HOME=" + filepath.Join(f.home, ".config"),
		"XDG_STATE_HOME=" + filepath.Join(f.home, ".local/state"),
		"XDG_DATA_HOME=" + filepath.Join(f.home, ".local/share"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(f.home, ".gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	}
	return append(env, f.extra...)
}

type result struct {
	stdout, stderr string
	code           int
}

func (r result) String() string {
	return fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
}

// with returns a copy that adds environment entries such as PI_PROJECT=garden.
func (f *fixture) with(env ...string) *fixture {
	copy := *f
	copy.extra = append(append([]string{}, f.extra...), env...)
	return &copy
}

func (f *fixture) input(text string) *fixture {
	copy := *f
	copy.stdin = text
	return &copy
}

// in returns a copy that runs the next command from dir.
func (f *fixture) in(dir string) *fixture {
	copy := *f
	copy.cwd = dir
	return &copy
}

func (f *fixture) exec(args ...string) result {
	f.t.Helper()
	command := exec.Command(implementation.command[0], append(implementation.command[1:], args...)...)
	command.Env = f.environment()
	command.Dir = f.home
	if f.cwd != "" {
		command.Dir = f.cwd
	}
	command.Stdin = strings.NewReader(f.stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	code := 0
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		f.t.Fatal(err)
	}
	return result{stdout.String(), stderr.String(), code}
}

// agent runs a user command such as agent ls.
func (f *fixture) agent(args ...string) result {
	return f.exec(args...)
}

// internal runs an entry point that SSH forced commands and Rex tabs use, such as --remote-inbox.
func (f *fixture) internal(args ...string) result { return f.exec(args...) }

// runner runs a project's generation loop, as its Rex tab does.
func (f *fixture) runner(project string) result {
	return f.exec("--run", project)
}

// operatorAsk sends a request as the operator from a laptop dispatch.
func (f *fixture) operatorAsk(target, text string) result {
	return f.exec("--client-operator", "ask", target, text)
}

func (f *fixture) ok(r result) result {
	f.t.Helper()
	if r.code != 0 {
		f.t.Fatalf("command failed\n%s", r)
	}
	return r
}

func (f *fixture) fails(r result, message string) {
	f.t.Helper()
	if r.code == 0 {
		f.t.Fatalf("command succeeded, want failure with %q\n%s", message, r)
	}
	if !strings.Contains(r.stderr, message) {
		f.t.Fatalf("stderr lacks %q\n%s", message, r)
	}
}

// open starts a project on this host and returns its workspace.
func (f *fixture) open(name string) string {
	f.t.Helper()
	f.ok(f.agent(name))
	return f.workspace(name)
}

func (f *fixture) path(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(f.home, rel)
}

func (f *fixture) write(rel, text string) {
	f.t.Helper()
	path := f.path(rel)
	must(f.t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(f.t, os.WriteFile(path, []byte(text), 0o644))
}

func (f *fixture) read(rel string) string {
	f.t.Helper()
	data, err := os.ReadFile(f.path(rel))
	must(f.t, err)
	return string(data)
}

func (f *fixture) exists(rel string) bool {
	_, err := os.Lstat(f.path(rel))
	return err == nil
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = f.environment()
	out, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// lock holds a project's or task's runner lock the way a live runner does, until release.
func (f *fixture) lock(rel string) (release func()) {
	f.t.Helper()
	path := f.path(rel)
	must(f.t, os.MkdirAll(filepath.Dir(path), 0o755))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	must(f.t, err)
	must(f.t, syscall.Flock(int(file.Fd()), syscall.LOCK_EX))
	released := false
	release = func() {
		if !released {
			released = true
			syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			file.Close()
		}
	}
	f.t.Cleanup(release)
	return release
}

// calls returns the logged invocations of one stub, oldest first.
func (f *fixture) calls(tool string) []call {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "calls.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	must(f.t, err)
	var out []call
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var c call
		must(f.t, json.Unmarshal([]byte(line), &c))
		if c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

func readJSON[T any](t *testing.T, path string) T {
	t.Helper()
	var value T
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, json.Unmarshal(data, &value))
	return value
}

func jsonFiles(t *testing.T, dir string) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	must(t, err)
	var items []map[string]any
	for _, path := range paths {
		items = append(items, readJSON[map[string]any](t, path))
	}
	return items
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	must(t, err)
	return string(data) + "\n"
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func init() {
	if runtime.GOOS == "windows" {
		panic("the agent platform needs a Unix host")
	}
}
