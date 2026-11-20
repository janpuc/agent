package tests

import (
	"slices"
	"testing"
)

// --model runs one task on another model than its project, the project itself keeps its own.
func TestTaskNewModelOverridesTheProjectModel(t *testing.T) {
	if implementation.kind == "python" {
		t.Skip("Go runner only")
	}
	f := newFixture(t, "hub")
	f.ok(f.agent("task", "new", "garden", "--model", "other/model-x:low", "--slug", "trial", "Try the other model"))
	f.ok(f.internal("--task-run", "garden", "trial"))
	calls := f.calls("pi")
	if len(calls) != 1 || !slices.Contains(calls[0].Args, "other/model-x:low") {
		t.Fatalf("task Pi starts %d, args %v", len(calls), calls)
	}
	f.ok(f.agent("task", "new", "garden", "--slug", "plain", "Same model as the project"))
	f.ok(f.internal("--task-run", "garden", "plain"))
	calls = f.calls("pi")
	if slices.Contains(calls[len(calls)-1].Args, "other/model-x:low") {
		t.Fatalf("override leaked into another task: %q", calls[len(calls)-1].Args)
	}
}
