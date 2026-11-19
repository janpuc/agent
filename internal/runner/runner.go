// Package runner runs the long-lived Pi loop behind a project's or task's Rex tab. It holds the
// runner lock, builds the environment Pi inherits and rotates to the next generation.
package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/workspace"
)

// Host is the execution host that runs a project.
type Host struct {
	Registry *registry.Registry
	Home     string
	Name     string
	Root     string
}

// providerKeys are the credentials each provider may use. A provider policy strips every key of
// the providers it does not name, so a session only holds the credentials it may use.
var providerKeys = map[string][]string{
	"claude-bridge": {"PI_CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
	"openai":        {"PI_OPENAI_API_KEY", "OPENAI_API_KEY"},
	"opencode-go":   {"PI_OPENCODE_API_KEY", "OPENCODE_API_KEY"},
	"minimax":       {"PI_MINIMAX_API_KEY", "MINIMAX_API_KEY"},
	"alibaba":       {"PI_ALIBABA_API_KEY", "ALIBABA_API_KEY"},
	"litellm":       {"LITELLM_API_KEY"},
}

// cwdEscape is the punctuation Pi replaces with a dash in its sessions tree.
var cwdEscape = regexp.MustCompile(`[/\\:]`)

// Loop runs one project's or task's generation loop and returns Pi's exit code. An empty task
// runs the project session; a task runs the task's own session in its folder.
func Loop(h Host, name, task string) (int, error) {
	folder := filepath.Join(h.Root, name)
	if task != "" {
		if _, err := workspace.ReadTask(folder, task); err != nil {
			return 0, err
		}
	}
	if err := os.Chdir(folder); err != nil {
		return 0, err
	}
	state := filepath.Join(folder, ".agent")
	if task != "" {
		state = workspace.TaskState(folder, task)
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		return 0, err
	}
	lock, err := lockRunner(name, state)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	env, cwd, err := environment(h, name, task, folder, state)
	if err != nil {
		return 0, err
	}
	// The terminal delivers SIGINT to Pi directly. The runner stays out of the way while it runs.
	if task == "" {
		if _, err := consumeRotation(folder, true); err != nil {
			return 0, err
		}
	}
	for {
		if task != "" && workspace.Exists(filepath.Join(state, "closed")) {
			return 0, nil
		}
		// A restart marker left before this launch is stale. Writer commands and the extension
		// write one during a run, and the check after Pi exits relaunches it.
		if err := os.Remove(filepath.Join(state, "restart-requested")); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		generation := 1
		if task == "" {
			if generation, err = generationOf(folder); err != nil {
				return 0, err
			}
			if !workspace.Exists(filepath.Join(state, "generation")) {
				if err := workspace.AtomicWrite(filepath.Join(state, "generation"),
					[]byte(strconv.Itoa(generation)+"\n")); err != nil {
					return 0, err
				}
			}
		}
		session := name + "-" + strconv.Itoa(generation)
		if task != "" {
			session = name + "-task-" + task
		}
		sessionDir := filepath.Join(h.Home, ".pi/agent/sessions",
			"--"+cwdEscape.ReplaceAllString(strings.TrimLeft(cwd, `/\`), "-")+"--")
		handoff := filepath.Join(state, "handoff-"+strconv.Itoa(generation)+".md")
		model, thinking := projectModel(h, name)
		args := []string{
			filepath.Join(h.Home, ".pi/agent/local/bin/pi"),
			"--session-id", session,
			"--model", model + ":" + thinking,
		}
		matches, err := filepath.Glob(filepath.Join(sessionDir, "*_"+session+".jsonl"))
		if err != nil {
			return 0, err
		}
		if len(matches) == 0 {
			if task != "" {
				args = append(args, "Work on task "+task+". Read BRIEF.md and STATE.md. Follow tasks.md. "+
					"Use branch task/"+task+" and .worktrees/<repo>/"+task+" only for repositories you need.")
			} else if workspace.Exists(handoff) {
				text, err := os.ReadFile(handoff)
				if err != nil {
					return 0, err
				}
				args = append(args, string(text))
			}
		}
		code, terminated, err := runPi(args, env, cwd)
		if err != nil {
			return 0, err
		}
		if terminated {
			return code, nil
		}
		if task == "" {
			rotated, err := consumeRotation(folder, false)
			if err != nil {
				return 0, err
			}
			if rotated {
				continue
			}
		}
		// A restart keeps the generation and session so the conversation resumes.
		if workspace.Exists(filepath.Join(state, "restart-requested")) {
			continue
		}
		return code, nil
	}
}

// lockRunner takes the non-blocking runner lock, clears the stale pid and records this process.
func lockRunner(name, state string) (*os.File, error) {
	path := filepath.Join(state, "run.lock")
	lock, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: agent-run is already running", name)
		}
		return nil, err
	}
	// Archive proves the Rex block belongs to this runner by reading this pid.
	if err := lock.Truncate(0); err != nil {
		lock.Close()
		return nil, err
	}
	if _, err := lock.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		lock.Close()
		return nil, err
	}
	// Pi must never inherit the lock that proves its runner is alive.
	syscall.CloseOnExec(int(lock.Fd()))
	return lock, nil
}

// environment is the environment Pi inherits and the directory it starts in.
func environment(h Host, name, task, folder, state string) ([]string, string, error) {
	values := environmentMap(os.Environ())
	workspaceEnv, err := miseEnv(folder)
	if err != nil {
		return nil, "", err
	}
	for key, value := range workspaceEnv {
		values[key] = value
	}
	project := h.Registry.Projects[name]
	host := h.Registry.Hosts[h.Name]
	guard := host.Guard
	if guard == "" {
		guard = "personal"
	}
	namespace := project.MeminiNamespace
	if namespace == "" {
		prefix := host.MeminiPrefix
		if prefix == "" {
			prefix = "homelab"
		}
		namespace = prefix + "/" + name
	}
	meminiHome := host.MeminiHome
	if meminiHome == "" {
		meminiHome = "none"
	}
	values["PI_PROJECT"] = name
	values["PI_PROJECT_DIR"] = folder
	values["PI_PROJECT_ROOT"] = h.Root
	values["PI_GUARD_PROFILE"] = guard
	values["MEMINI_NAMESPACE"] = namespace
	values["MEMINI_HOME"] = meminiHome
	for _, key := range []string{"MEMINI_NAMESPACE_PREFIX", "PI_TASK", "PI_TASK_DIR", "PI_TASK_STATE_DIR",
		"OP_SERVICE_ACCOUNT_TOKEN"} {
		delete(values, key)
	}
	if values["MEMINI_HOME"] == "none" {
		delete(values, "MEMINI_HOME")
	}
	cwd := folder
	if task != "" {
		meta, err := workspace.ReadTask(folder, task)
		if err != nil {
			return nil, "", err
		}
		if meta.Status == "closed" {
			return nil, "", fmt.Errorf("%s: closed", task)
		}
		if cwd, err = workspace.TaskFolder(folder, task); err != nil {
			return nil, "", err
		}
		values["PI_TASK"] = task
		values["PI_TASK_DIR"] = cwd
		values["PI_TASK_STATE_DIR"] = state
		if err := os.Chdir(cwd); err != nil {
			return nil, "", err
		}
	}
	if err := projectSecrets(h, name, project, values); err != nil {
		return nil, "", err
	}
	if err := providerPolicy(h, name, project, values); err != nil {
		return nil, "", err
	}
	// Equivalent to `mise activate bash --shims`. Unlike real tool bin paths, a shim resolves
	// tools and [env] from each command's cwd, including nested repositories.
	dataDir := values["MISE_DATA_DIR"]
	if dataDir == "" {
		dataDir = filepath.Join(h.Home, ".local/share/mise")
	}
	values["PATH"] = filepath.Join(dataDir, "shims") + string(os.PathListSeparator) + values["PATH"]
	return environmentList(values), cwd, nil
}

// projectSecrets loads the project's dotenv file when the registry names secrets.
func projectSecrets(h Host, name string, project registry.Project, values map[string]string) error {
	if len(project.Secrets) == 0 {
		return nil
	}
	path := filepath.Join(h.Home, ".local/state/ai/projects", name+".env")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	loaded := map[string]bool{}
	// ai-sync writes JSON-quoted dotenv values with dollars escaped against expansion.
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := []string{}
	if text != "" {
		lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	for _, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("invalid secret line in %s", path)
		}
		if _, expected := project.Secrets[key]; !expected {
			return fmt.Errorf("Unexpected secret key %s in %s", key, path)
		}
		var decoded string
		if err := json.Unmarshal([]byte(strings.ReplaceAll(value, `\$`, "$")), &decoded); err != nil {
			return err
		}
		values[key] = decoded
		loaded[key] = true
	}
	for key := range project.Secrets {
		if !loaded[key] {
			return fmt.Errorf("Missing project secrets, run ai-sync: %s", path)
		}
	}
	return nil
}

// providerPolicy strips the credentials of every provider the host or project policy does not
// allow. It is a security boundary, so an unknown policy or provider is an error.
func providerPolicy(h Host, name string, project registry.Project, values map[string]string) error {
	host := h.Registry.Hosts[h.Name]
	policies := []any{host.Providers, project.Providers}
	if project.Providers == nil {
		policies[1] = h.Registry.Defaults.Providers
	}
	for _, policy := range policies {
		if policy == nil {
			policy = "all"
		}
		allowed := map[string]bool{}
		switch value := policy.(type) {
		case string:
			if value == "all" {
				continue
			}
			return errors.New("Invalid providers policy")
		case []any:
			for _, provider := range value {
				name, ok := provider.(string)
				if !ok {
					return errors.New("Invalid providers policy")
				}
				if _, ok := providerKeys[name]; !ok {
					return errors.New("Invalid providers policy")
				}
				allowed[name] = true
			}
		case []string:
			for _, name := range value {
				if _, ok := providerKeys[name]; !ok {
					return errors.New("Invalid providers policy")
				}
				allowed[name] = true
			}
		default:
			return errors.New("Invalid providers policy")
		}
		for provider, keys := range providerKeys {
			if allowed[provider] {
				continue
			}
			for _, key := range keys {
				delete(values, key)
			}
		}
	}
	return nil
}

// projectModel is the model and thinking level Pi starts with.
func projectModel(h Host, name string) (string, string) {
	project := h.Registry.Projects[name]
	model, thinking := project.Model, project.Thinking
	if model == "" {
		model = h.Registry.Defaults.Model
	}
	if thinking == "" {
		thinking = h.Registry.Defaults.Thinking
	}
	return model, thinking
}

// miseEnv runs `mise env --json` in the workspace.
func miseEnv(folder string) (map[string]string, error) {
	command := exec.Command("mise", "env", "--json")
	command.Dir = folder
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := json.Unmarshal(output, &values); err != nil {
		return nil, err
	}
	env := map[string]string{}
	for key, value := range values {
		env[key] = fmt.Sprint(value)
	}
	return env, nil
}

// runPi starts Pi and forwards termination signals to it. It reports the child's exit code and
// whether a forwarded signal ended the loop.
func runPi(args, env []string, cwd string) (int, bool, error) {
	command := exec.Command(args[0], args[1:]...)
	command.Env = env
	command.Dir = cwd
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return 0, false, err
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	done := make(chan struct{})
	terminated := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case received := <-signals:
				if received == os.Interrupt {
					// Pi gets the interrupt from the terminal itself.
					continue
				}
				command.Process.Signal(received)
				select {
				case terminated <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	err := command.Wait()
	close(done)
	code, err := exitCode(err)
	if err != nil {
		return 0, false, err
	}
	select {
	case <-terminated:
		return code, true, nil
	default:
		return code, false, nil
	}
}

// exitCode is a child's exit code. A child killed by a signal reports the negation of the
// signal, as Python's subprocess does.
func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return 0, err
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -int(status.Signal()), nil
	}
	return exit.ExitCode(), nil
}

// generationOf reads the project's generation. A workspace without the file is generation 1.
func generationOf(folder string) (int, error) {
	path := filepath.Join(folder, ".agent", "generation")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || value < 1 {
		return 0, fmt.Errorf("Invalid generation in %s", path)
	}
	return value, nil
}

// consumeRotation commits a requested rotation. With recovering set, a request whose generation
// was already committed is finished instead of refused. An invalid request changes nothing.
func consumeRotation(folder string, recovering bool) (bool, error) {
	state := filepath.Join(folder, ".agent")
	requested := filepath.Join(state, "rotate-requested")
	if !workspace.Exists(requested) {
		return false, nil
	}
	data, err := os.ReadFile(requested)
	if err != nil {
		return false, err
	}
	next, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false, err
	}
	current, err := generationOf(folder)
	if err != nil {
		return false, err
	}
	handoff := filepath.Join(state, "handoff-"+strconv.Itoa(next)+".md")
	info, err := os.Stat(handoff)
	if err != nil || !info.Mode().IsRegular() {
		return false, errors.New("Rotation requested without a handoff")
	}
	if next == current && recovering {
		// The previous runner committed generation but stopped before removing the request.
		if err := os.Remove(requested); err != nil {
			return false, err
		}
		return true, nil
	}
	if next != current+1 {
		return false, errors.New("Rotation request must name the next generation")
	}
	if err := workspace.AtomicWrite(filepath.Join(state, "generation"),
		[]byte(strconv.Itoa(next)+"\n")); err != nil {
		return false, err
	}
	if err := os.Remove(requested); err != nil {
		return false, err
	}
	return true, nil
}

// environmentMap turns the process environment into a map.
func environmentMap(entries []string) map[string]string {
	values := map[string]string{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

// environmentList renders an environment in stable key order.
func environmentList(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]string, len(keys))
	for i, key := range keys {
		entries[i] = key + "=" + values[key]
	}
	return entries
}
