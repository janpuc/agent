package tests

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunnerAppliesHostAndProjectProviderPolicies checks the security boundary that decides
// which provider credentials reach Pi. Both policies narrow the environment in turn.
func TestRunnerAppliesHostAndProjectProviderPolicies(t *testing.T) {
	f := newFixture(t, "hub")
	f.open("garden")
	f.registry["hosts"].(map[string]any)["hub"].(map[string]any)["providers"] = []any{"openai", "litellm"}
	f.project("garden")["providers"] = []any{"openai"}
	f.saveRegistry()
	env := []string{
		"PI_OPENAI_API_KEY=openai", "OPENAI_API_KEY=openai",
		"PI_CLAUDE_CODE_OAUTH_TOKEN=claude", "CLAUDE_CODE_OAUTH_TOKEN=claude", "ANTHROPIC_API_KEY=claude",
		"PI_OPENCODE_API_KEY=opencode", "OPENCODE_API_KEY=opencode",
		"PI_MINIMAX_API_KEY=minimax", "MINIMAX_API_KEY=minimax",
		"PI_ALIBABA_API_KEY=alibaba", "ALIBABA_API_KEY=alibaba",
		"LITELLM_API_KEY=litellm",
	}
	f.ok(f.with(env...).runner("garden"))
	calls := f.calls("pi")
	if len(calls) != 1 {
		t.Fatalf("want one Pi start, got %d", len(calls))
	}
	piEnv := calls[0].Env
	for _, key := range []string{"PI_OPENAI_API_KEY", "OPENAI_API_KEY"} {
		if piEnv[key] == "" {
			t.Fatalf("%s was stripped by the policies", key)
		}
	}
	for _, key := range []string{"PI_CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY",
		"PI_OPENCODE_API_KEY", "OPENCODE_API_KEY", "PI_MINIMAX_API_KEY", "MINIMAX_API_KEY",
		"PI_ALIBABA_API_KEY", "ALIBABA_API_KEY", "LITELLM_API_KEY"} {
		if _, set := piEnv[key]; set {
			t.Fatalf("%s survived the policies", key)
		}
	}
}

func TestRunnerRefusesAnInvalidProviderPolicy(t *testing.T) {
	f := newFixture(t, "hub")
	f.open("garden")
	f.project("garden")["providers"] = "openai"
	f.saveRegistry()
	f.fails(f.runner("garden"), "Invalid providers policy")
	if len(f.calls("pi")) != 0 {
		t.Fatal("Pi started with an invalid provider policy")
	}
	f.project("garden")["providers"] = []any{"openai", "unknown"}
	f.saveRegistry()
	f.fails(f.runner("garden"), "Invalid providers policy")
}

// TestRunnerResetsPlatformOnlyEnvironment checks that a nested agent session cannot inherit the
// outer session's identity, namespace prefix or service account token.
func TestRunnerResetsPlatformOnlyEnvironment(t *testing.T) {
	f := newFixture(t, "hub")
	f.open("garden")
	delete(f.registry["hosts"].(map[string]any)["hub"].(map[string]any), "meminiHome")
	f.saveRegistry()
	f.ok(f.with(
		"MEMINI_NAMESPACE_PREFIX=outer",
		"PI_TASK=outer-task",
		"PI_TASK_DIR=/tmp/outer-task",
		"PI_TASK_STATE_DIR=/tmp/outer-state",
		"OP_SERVICE_ACCOUNT_TOKEN=secret",
	).runner("garden"))
	calls := f.calls("pi")
	if len(calls) != 1 {
		t.Fatalf("want one Pi start, got %d", len(calls))
	}
	piEnv := calls[0].Env
	for _, key := range []string{"MEMINI_NAMESPACE_PREFIX", "PI_TASK", "PI_TASK_DIR", "PI_TASK_STATE_DIR",
		"OP_SERVICE_ACCOUNT_TOKEN", "MEMINI_HOME"} {
		if _, set := piEnv[key]; set {
			t.Fatalf("%s reached Pi", key)
		}
	}
	if piEnv["MEMINI_NAMESPACE"] != "lab/garden" {
		t.Fatalf("MEMINI_NAMESPACE %q", piEnv["MEMINI_NAMESPACE"])
	}
}

func TestTaskRunUsesTheTaskEnvironmentAndFolder(t *testing.T) {
	f := newFixture(t, "hub")
	f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	garden := f.workspace("garden")
	target := filepath.Join(garden, "tasks/audit")
	state := filepath.Join(garden, ".agent/tasks/audit")
	f.ok(f.internal("--task-run", "garden", "audit"))
	calls := f.calls("pi")
	if len(calls) != 1 {
		t.Fatalf("want one Pi start, got %d", len(calls))
	}
	pi := calls[0]
	if pi.Cwd != target {
		t.Fatalf("Pi cwd %q, want %q", pi.Cwd, target)
	}
	for key, want := range map[string]string{
		"PI_PROJECT":        "garden",
		"PI_PROJECT_DIR":    garden,
		"PI_TASK":           "audit",
		"PI_TASK_DIR":       target,
		"PI_TASK_STATE_DIR": state,
		"MEMINI_NAMESPACE":  "lab/garden",
	} {
		if pi.Env[key] != want {
			t.Fatalf("%s %q, want %q", key, pi.Env[key], want)
		}
	}
	if flag(pi.Args, "--session-id") != "garden-task-audit" {
		t.Fatalf("session id %q", pi.Args)
	}
	want := "Work on task audit. Read BRIEF.md and STATE.md. Follow tasks.md. Use branch task/audit " +
		"and .worktrees/<repo>/audit only for repositories you need."
	if lastArg(pi) != want {
		t.Fatalf("first prompt %q", lastArg(pi))
	}
}

func TestClosedTaskLoopEndsWithoutStartingPi(t *testing.T) {
	f := newFixture(t, "hub")
	f.ok(f.agent("task", "new", "garden", "--slug", "audit", "Audit the speakers"))
	garden := f.workspace("garden")
	metaPath := filepath.Join(garden, ".agent/tasks/audit/meta.json")
	meta := readJSON[map[string]any](t, metaPath)
	meta["status"] = "closing"
	f.write(metaPath, mustJSON(t, meta))
	f.write(filepath.Join(garden, ".agent/tasks/audit/closed"), "2026-11-19T10:00:00Z\n")
	f.ok(f.internal("--task-run", "garden", "audit"))
	if len(f.calls("pi")) != 0 {
		t.Fatal("Pi started for a closed task")
	}
}

// TestClientDispatchCommandLines checks the SSH command lines a client builds. The client runs
// no agent command itself; every target runs its own Go command.
func TestClientDispatchCommandLines(t *testing.T) {
	f := newFixture(t, "laptop")
	program, operator := "agent", "--client-operator"
	if implementation.kind == "python" {
		program, operator = "python3 .local/bin/agent-run", "--client-jan"
	}
	f.ok(f.internal("--launch", "ls"))
	f.ok(f.internal("--launch", "ask", "garden", "Please", "check"))
	f.ok(f.with("PI_PROJECT=garden").internal("--launch", "task", "new", "--slug", "audit", "Audit"))
	calls := f.calls("ssh")
	want := []struct{ host, remote string }{
		{"hub", "env -u PI_PROJECT fish -lc '" + program + " --launch ls'"},
		{"outpost", "env -u PI_PROJECT fish -lc '" + program + " --launch ls'"},
		{"hub", "env -u PI_PROJECT fish -lc '" + program + " " + operator + " --launch ask garden Please check'"},
		{"hub", "env PI_PROJECT=garden fish -lc '" + program + " --launch task new garden --slug audit Audit'"},
	}
	if len(calls) != len(want) {
		t.Fatalf("ssh calls %v", calls)
	}
	for i, expected := range want {
		if calls[i].Args[0] != expected.host || calls[i].Args[1] != expected.remote {
			t.Fatalf("call %d: %q, want %q", i, calls[i].Args, []string{expected.host, expected.remote})
		}
	}
}

func TestDigestDailyWritesTodaysDigest(t *testing.T) {
	f := newFixture(t, "hub")
	path := strings.TrimSpace(f.ok(f.internal("--digest-daily")).stdout)
	want := f.path(".local/state/agents/digest/" + time.Now().Format("2006-01-02") + ".md")
	if path != want {
		t.Fatalf("digest path %q, want %q", path, want)
	}
	if text := f.read(path); !strings.HasPrefix(text, "# hub digest, last 24 hours") {
		t.Fatalf("daily digest\n%s", text)
	}
}

func TestHelpIsPrintedOnStdout(t *testing.T) {
	f := newFixture(t, "hub")
	for _, args := range [][]string{{}, {"--help"}, {"help"}} {
		r := f.ok(f.exec(args...))
		if strings.TrimSpace(r.stderr) != "" {
			t.Fatalf("%v wrote to stderr: %s", args, r.stderr)
		}
		for _, want := range []string{"Projects", "Requests", "Overview", "Tasks",
			"agent ls", "ask <project> <text>", "task new [project]"} {
			if !strings.Contains(r.stdout, want) {
				t.Fatalf("%v help lacks %q\n%s", args, want, r.stdout)
			}
		}
	}
	// The dispatched forms print the same guide, including task without a subcommand.
	for _, args := range [][]string{{"--launch", "task"}, {"--launch", "help"}} {
		r := f.ok(f.internal(args...))
		if !strings.Contains(r.stdout, "task new [project]") {
			t.Fatalf("%v did not print the guide\n%s", args, r.stdout)
		}
	}
}

// TestUsageErrorsPrintOnlyTheirUsageLine checks that a wrong command prints its Usage line and a
// pointer to the guide, not the whole grouped guide.
func TestUsageErrorsPrintOnlyTheirUsageLine(t *testing.T) {
	if implementation.kind != "go" {
		t.Skip("the Python runner still appends the guide to a usage error")
	}
	f := newFixture(t, "hub")
	for _, args := range [][]string{{"reply"}, {"ask"}, {"todo", "extra"}, {"--launch", "open"}} {
		r := f.exec(args...)
		if r.code == 0 {
			t.Fatalf("%v succeeded, want a usage error\n%s", args, r)
		}
		if !(strings.Contains(r.stderr, "Usage: agent ") || strings.Contains(r.stderr, "usage: agent ")) || !strings.Contains(r.stderr, "Run agent --help for all commands.") {
			t.Fatalf("%v stderr lacks the usage line or the hint\n%s", args, r)
		}
		if strings.Contains(r.stderr, "Projects\n") || strings.Contains(r.stderr, "task new [project]") {
			t.Fatalf("%v printed the whole guide\n%s", args, r)
		}
	}
}

// TestRunnerRefusesANullProviderPolicy checks that an explicit null is invalid wherever it is
// set, instead of being read as an absent policy that allows every provider.
func TestRunnerRefusesANullProviderPolicy(t *testing.T) {
	for _, where := range []string{"host", "project", "defaults"} {
		t.Run(where, func(t *testing.T) {
			f := newFixture(t, "hub")
			f.open("garden")
			switch where {
			case "host":
				f.registry["hosts"].(map[string]any)["hub"].(map[string]any)["providers"] = nil
			case "project":
				f.project("garden")["providers"] = nil
			case "defaults":
				delete(f.project("garden"), "providers")
				f.registry["defaults"].(map[string]any)["providers"] = nil
			}
			f.saveRegistry()
			f.fails(f.with("OPENAI_API_KEY=openai").runner("garden"), "Invalid providers policy")
			if len(f.calls("pi")) != 0 {
				t.Fatal("Pi started with a null provider policy")
			}
		})
	}
}

// TestRunnerRefusesANullSecret checks that a secret must be a JSON string. null is not an
// empty value.
func TestRunnerRefusesANullSecret(t *testing.T) {
	f := newFixture(t, "hub")
	f.project("garden")["secrets"] = map[string]any{"HA_TOKEN": "op://vault/item/field"}
	f.saveRegistry()
	f.open("garden")
	f.write(".local/state/ai/projects/garden.env", "HA_TOKEN=null\n")
	r := f.runner("garden")
	if r.code == 0 || len(f.calls("pi")) != 0 {
		t.Fatalf("Pi started with a null secret\n%s", r)
	}
	if implementation.kind != "python" && !strings.Contains(r.stderr, "Invalid secret value for HA_TOKEN") {
		t.Fatalf("stderr %s", r.stderr)
	}
}

// TestRunnerTreatsBracketsInTheRootAsLiteral checks that the session lookup does not read a
// workspace path as a glob pattern, which could hide a session and replay its handoff.
func TestRunnerTreatsBracketsInTheRootAsLiteral(t *testing.T) {
	for _, root := range []string{"[team]", "[open"} {
		t.Run(root, func(t *testing.T) {
			f := newFixture(t, "hub")
			f.registry["hosts"].(map[string]any)["hub"].(map[string]any)["root"] = "~/" + root
			f.saveRegistry()
			f.ok(f.agent("garden"))
			garden := filepath.Join(f.home, root, "garden")
			f.write(filepath.Join(garden, ".agent/handoff-1.md"), "replayed handoff")
			sessions := f.path(".pi/agent/sessions/--" + strings.ReplaceAll(strings.TrimPrefix(garden, "/"), "/", "-") + "--")
			f.write(filepath.Join(sessions, "2026-11-19T10-00-00_garden-1.jsonl"), "")
			f.ok(f.runner("garden"))
			calls := f.calls("pi")
			if len(calls) != 1 || lastArg(calls[0]) == "replayed handoff" {
				t.Fatalf("pi calls %v", calls)
			}
		})
	}
}

// TestProjectWithoutAnyHostRunsOnTheHub checks the fallback when neither the project nor the
// defaults name a host.
func TestProjectWithoutAnyHostRunsOnTheHub(t *testing.T) {
	f := newFixture(t, "hub")
	delete(f.registry["defaults"].(map[string]any), "host")
	for _, name := range []string{"garden"} {
		delete(f.project(name), "host")
	}
	f.saveRegistry()
	f.open("garden")
	f.ok(f.runner("garden"))
	if len(f.calls("pi")) != 1 {
		t.Fatal("a project without a host did not run on the hub")
	}
}
