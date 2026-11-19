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
	program := "agent"
	if implementation.kind == "python" {
		program = "python3 .local/bin/agent-run"
	}
	f.ok(f.internal("--launch", "ls"))
	f.ok(f.internal("--launch", "ask", "garden", "Please", "check"))
	f.ok(f.with("PI_PROJECT=garden").internal("--launch", "task", "new", "--slug", "audit", "Audit"))
	calls := f.calls("ssh")
	want := []struct{ host, remote string }{
		{"hub", "env -u PI_PROJECT fish -lc '" + program + " --launch ls'"},
		{"outpost", "env -u PI_PROJECT fish -lc '" + program + " --launch ls'"},
		{"hub", "env -u PI_PROJECT fish -lc '" + program + " --client-jan --launch ask garden Please check'"},
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
