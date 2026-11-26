package tests

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func (f *fixture) piScript(actions ...piAction) {
	f.write(filepath.Join(f.dir, "pi-script.json"), mustJSON(f.t, actions))
}

func lastArg(c call) string { return c.Args[len(c.Args)-1] }

func TestListShowsGenerationAndRunnerLock(t *testing.T) {
	f := newFixture(t, "hub")
	garden := f.open("garden")
	line := regexp.MustCompile(`(?m)^garden .*$`)
	listing := line.FindString(f.ok(f.agent("ls")).stdout)
	if !strings.Contains(listing, "init=initialized") || !strings.Contains(listing, "pi=no") ||
		!strings.Contains(listing, "generation=1") || !strings.Contains(listing, "rex=session:") {
		t.Fatalf("listing %q", listing)
	}
	release := f.lock(filepath.Join(garden, ".agent/run.lock"))
	if listing := line.FindString(f.ok(f.agent("ls")).stdout); !strings.Contains(listing, "pi=yes") {
		t.Fatalf("held runner lock not shown: %q", listing)
	}
	release()
	if listing := line.FindString(f.ok(f.agent("ls")).stdout); !strings.Contains(listing, "pi=no") {
		t.Fatalf("released runner lock still shown: %q", listing)
	}
	f.write(filepath.Join(garden, ".agent/generation"), "0\n")
	f.fails(f.agent("ls"), "generation")
}

func TestOpenCreatesSessionOnceAndRestartsInExistingTab(t *testing.T) {
	f := newFixture(t, "hub")
	garden := f.open("garden")
	creates := func() (n int) {
		for _, c := range f.calls("rex") {
			if len(c.Args) > 2 && c.Args[2] == "session.create" {
				n++
			}
		}
		return n
	}
	runs := func() (out []call) {
		for _, c := range f.calls("rex") {
			if c.Args[0] == "run" {
				out = append(out, c)
			}
		}
		return out
	}
	state := loadRexAt(f)
	if creates() != 1 || len(state.Sessions) != 1 || len(state.Sessions[0].Windows) != 2 {
		t.Fatalf("first open: %d creates, state %+v", creates(), state)
	}
	pi, shell := state.Sessions[0].Windows[0], state.Sessions[0].Windows[1]
	if pi.Label != "" || shell.Label != "shell" {
		t.Fatalf("window labels %q and %q, want an unlabelled pi tab and shell", pi.Label, shell.Label)
	}
	if strings.TrimSpace(f.read(filepath.Join(garden, ".agent/rex-window"))) != pi.ID {
		t.Fatalf("rex-window does not name the pi tab %s", pi.ID)
	}
	// A live runner keeps its tab.
	release := f.lock(filepath.Join(garden, ".agent/run.lock"))
	f.open("garden")
	if creates() != 1 || len(runs()) != 0 {
		t.Fatalf("open with a live runner: %d creates, %d runs", creates(), len(runs()))
	}
	release()
	// A stopped runner is replaced inside the same tab, never in a new one.
	f.open("garden")
	if creates() != 1 || len(runs()) != 1 {
		t.Fatalf("open with a stopped runner: %d creates, %d runs", creates(), len(runs()))
	}
	run := runs()[0]
	if !contains(run.Args, "--split=right") || !contains(run.Args, "--keep-open") || flag(run.Args, "-b") != pi.Layers[0].Blocks[0].ID {
		t.Fatalf("replacement %q", run.Args)
	}
	state = loadRexAt(f)
	blocks := state.Sessions[0].Windows[0].Layers[0].Blocks
	if len(state.Sessions[0].Windows) != 2 || len(blocks) != 1 || blocks[0].ID == pi.Layers[0].Blocks[0].ID {
		t.Fatalf("after replacement %+v", state.Sessions[0].Windows)
	}
	// A pi tab with a live process the runner did not start is left alone.
	state.Process = map[string]any{blocks[0].ID: map[string]any{
		"child": map[string]any{"name": "vim", "pid": 7}, "foreground": map[string]any{"pid": 7}}}
	state.saveAt(f)
	f.fails(f.agent("garden"), "unmanaged")
}

func TestRotationPassesHandoffOnlyToNewSession(t *testing.T) {
	f := newFixture(t, "hub")
	f.project("garden")["secrets"] = map[string]any{"HA_TOKEN": "op://vault/item/field"}
	f.saveRegistry()
	garden := f.open("garden")
	// The credential sync writes JSON-quoted values with dollars escaped against expansion.
	token := `test-$HOME-żółć-\$quoted`
	quoted := strings.ReplaceAll(strings.TrimSpace(mustJSONCompact(t, token)), "$", `\$`)
	f.write(".local/state/ai/projects/garden.env", "HA_TOKEN="+quoted+"\n")
	f.write(filepath.Join(f.dir, "mise-env.json"), `{"HA_URL": "https://hass.example"}`)
	state := filepath.Join(garden, ".agent")
	f.write(filepath.Join(state, "handoff-1.md"), "first handoff")
	f.piScript(piAction{Write: map[string]string{
		filepath.Join(state, "handoff-2.md"):     "second handoff",
		filepath.Join(state, "rotate-requested"): "2\n",
	}})
	f.ok(f.runner("garden"))
	calls := f.calls("pi")
	if len(calls) != 2 {
		t.Fatalf("want two Pi starts, got %d", len(calls))
	}
	first, second := calls[0], calls[1]
	if first.Generation != "1\n" || lastArg(first) != "first handoff" || !contains(first.Args, "garden-1") {
		t.Fatalf("first start %+v", first)
	}
	if lastArg(second) != "second handoff" || !contains(second.Args, "garden-2") {
		t.Fatalf("second start %q", second.Args)
	}
	if flag(first.Args, "--model") != "test/model:high" || flag(first.Args, "--session-id") != "garden-1" {
		t.Fatalf("model and session %q", first.Args)
	}
	env := first.Env
	if env["HA_TOKEN"] != token || env["HA_URL"] != "https://hass.example" || env["MEMINI_NAMESPACE"] != "lab/garden" ||
		env["PI_PROJECT"] != "garden" || env["PI_PROJECT_DIR"] != garden || env["PI_PROJECT_ROOT"] != f.root() ||
		env["PI_GUARD_PROFILE"] != "personal" {
		t.Fatalf("Pi environment %v", env)
	}
	if _, set := env["MEMINI_HOME"]; set {
		t.Fatal("meminiHome none must leave MEMINI_HOME unset")
	}
	if strings.Split(env["PATH"], ":")[0] != f.path(".local/share/mise/shims") || first.Cwd != garden {
		t.Fatalf("PATH %s, cwd %s", env["PATH"], first.Cwd)
	}
	if f.exists(filepath.Join(state, "rotate-requested")) || f.read(filepath.Join(state, "generation")) != "2\n" {
		t.Fatal("rotation not committed")
	}
	// A restart after a reboot resumes the existing session without replaying the handoff, and
	// finishes removing a request whose generation was already committed.
	sessions := f.path(".pi/agent/sessions/--" + strings.ReplaceAll(strings.TrimPrefix(garden, "/"), "/", "-") + "--")
	f.write(filepath.Join(sessions, "2026-11-19T10-00-00_garden-2.jsonl"), "")
	f.write(filepath.Join(state, "rotate-requested"), "2\n")
	f.ok(f.runner("garden"))
	calls = f.calls("pi")
	if len(calls) != 3 || lastArg(calls[2]) == "second handoff" || !contains(calls[2].Args, "garden-2") {
		t.Fatalf("resumed start %q", calls[len(calls)-1].Args)
	}
	if f.exists(filepath.Join(state, "rotate-requested")) {
		t.Fatal("committed rotation request left behind")
	}
}

func TestRunnerRefusesMissingOrMalformedSecrets(t *testing.T) {
	f := newFixture(t, "hub")
	f.project("garden")["secrets"] = map[string]any{"HA_TOKEN": "op://vault/item/field"}
	f.saveRegistry()
	f.open("garden")
	f.fails(f.runner("garden"), "garden.env")
	f.write(".local/state/ai/projects/garden.env", `OTHER="x"`+"\n")
	f.fails(f.runner("garden"), "Unexpected secret key")
	if len(f.calls("pi")) != 0 {
		t.Fatal("Pi started without its secrets")
	}
}

func TestRotationRejectsInvalidRequestWithoutChangingGeneration(t *testing.T) {
	f := newFixture(t, "hub")
	state := filepath.Join(f.open("garden"), ".agent")
	f.write(filepath.Join(state, "generation"), "1\n")
	request := filepath.Join(state, "rotate-requested")
	// Handoffs exist, so only the generation check can refuse 0 and 3.
	for _, n := range []string{"0", "3"} {
		f.write(filepath.Join(state, "handoff-"+n+".md"), "handoff "+n)
	}
	for _, value := range []string{"", "garbage", "0", "1", "3"} {
		f.write(request, value)
		if r := f.runner("garden"); r.code == 0 {
			t.Fatalf("rotation request %q accepted\n%s", value, r)
		}
		if f.read(filepath.Join(state, "generation")) != "1\n" || f.read(request) != value {
			t.Fatalf("request %q changed state", value)
		}
	}
	f.write(request, "2\n")
	f.fails(f.runner("garden"), "without a handoff")
	f.write(request, "3\n")
	f.fails(f.runner("garden"), "next generation")
	if f.read(filepath.Join(state, "generation")) != "1\n" || !f.exists(request) {
		t.Fatal("request without a handoff changed state")
	}
	if len(f.calls("pi")) != 0 {
		t.Fatal("Pi started with an invalid rotation request")
	}
}

func TestStartupConsumesPendingRotation(t *testing.T) {
	f := newFixture(t, "hub")
	state := filepath.Join(f.open("garden"), ".agent")
	f.write(filepath.Join(state, "generation"), "1\n")
	f.write(filepath.Join(state, "handoff-2.md"), "second handoff")
	f.write(filepath.Join(state, "rotate-requested"), "2\n")
	f.ok(f.runner("garden"))
	calls := f.calls("pi")
	if len(calls) != 1 || !contains(calls[0].Args, "garden-2") || lastArg(calls[0]) != "second handoff" {
		t.Fatalf("Pi starts %+v", calls)
	}
	if f.read(filepath.Join(state, "generation")) != "2\n" || f.exists(filepath.Join(state, "rotate-requested")) {
		t.Fatal("pending rotation not consumed")
	}
}

func TestStartupRecoversCommittedGeneration(t *testing.T) {
	f := newFixture(t, "hub")
	state := filepath.Join(f.open("garden"), ".agent")
	// The previous runner committed generation 2 and stopped before removing its request.
	f.write(filepath.Join(state, "generation"), "2\n")
	f.write(filepath.Join(state, "handoff-2.md"), "second handoff")
	f.write(filepath.Join(state, "rotate-requested"), "2\n")
	f.ok(f.runner("garden"))
	calls := f.calls("pi")
	if len(calls) != 1 || !contains(calls[0].Args, "garden-2") {
		t.Fatalf("Pi starts %+v", calls)
	}
	if f.read(filepath.Join(state, "generation")) != "2\n" || f.exists(filepath.Join(state, "rotate-requested")) {
		t.Fatal("committed rotation not recovered")
	}
}

func TestRestartKeepsSessionAndRelaunches(t *testing.T) {
	f := newFixture(t, "hub")
	state := filepath.Join(f.open("garden"), ".agent")
	// A restart marker left before launch is stale, one written during a run relaunches Pi.
	f.write(filepath.Join(state, "restart-requested"), "stale\n")
	f.piScript(piAction{Write: map[string]string{filepath.Join(state, "restart-requested"): "now\n"}}, piAction{Exit: 3})
	r := f.runner("garden")
	calls := f.calls("pi")
	if r.code != 3 || len(calls) != 2 || !contains(calls[1].Args, "garden-1") {
		t.Fatalf("restart: exit %d, starts %+v", r.code, calls)
	}
	release := f.lock(filepath.Join(state, "run.lock"))
	f.fails(f.runner("garden"), "already running")
	out := f.ok(f.agent("restart", "all")).stdout
	if !strings.Contains(out, "garden") || strings.TrimSpace(f.read(filepath.Join(state, "restart-requested"))) == "now" {
		t.Fatalf("restart all with a live runner: %s", out)
	}
	release()
}

func TestRunnerRefusesUninitializedAndArchivedProjects(t *testing.T) {
	f := newFixture(t, "hub")
	f.fails(f.runner("garden"), "initialization or restore needed")
	f.open("garden")
	f.project("garden")["lifecycle"] = "archived"
	f.saveRegistry()
	f.fails(f.runner("garden"), "archived")
	f.fails(f.agent("garden"), "archived")
	if len(f.calls("pi")) != 0 {
		t.Fatal("Pi started for a refused project")
	}
}

func TestTodoListsPendingApprovals(t *testing.T) {
	f := newFixture(t, "hub")
	garden := f.open("garden")
	f.write(filepath.Join(garden, "notes/STATE.md"), "# State\n\n## Current work\n\n- not a question\n\n"+
		"## Pending approvals\n\n- Buy seeds. Recommend yes.\n  Approver: alice.\n- Second\n\n## Next actions\n\n- later\n")
	out := f.ok(f.agent("todo", "--all")).stdout
	if out != "garden: Buy seeds. Recommend yes. Approver: alice.\ngarden: Second\n" {
		t.Fatalf("todo output %q", out)
	}
}

func loadRexAt(f *fixture) *rexState {
	f.t.Setenv("AGENT_TEST_DIR", f.dir)
	return loadRex()
}

func (s *rexState) saveAt(f *fixture) {
	f.t.Setenv("AGENT_TEST_DIR", f.dir)
	s.save()
}

func mustJSONCompact(t *testing.T, value any) string {
	t.Helper()
	text := mustJSON(t, value)
	return strings.TrimSpace(text)
}
