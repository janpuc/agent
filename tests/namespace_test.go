package tests

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNamespaceNeverDerivesAnUnregisteredName(t *testing.T) {
	cases := []struct{ host, dir, want string }{
		{"hub", "Projects/garden", "lab/garden"},
		{"hub", "Projects/garden/tasks/audit", "lab/garden"},
		{"hub", "Projects/unknown/repo", "lab/scratch"},
		{"hub", "Projects", "lab/scratch"},
		{"hub", "outside/repo", "lab/scratch"},
		{"hub", ".", "lab/scratch"},
		// A host never selects another host's namespace.
		{"hub", "Field/field", "lab/scratch"},
		{"outpost", "Field/field", "field"},
		{"outpost", "Field/draft", "field/scratch"},
		{"outpost", "Projects/garden", "field/scratch"},
		{"outpost", ".", "field/scratch"},
		// A client maps every host's root and falls back to the hub's scratch namespace.
		{"laptop", "Projects/garden", "lab/garden"},
		{"laptop", "Field/field", "field"},
		{"laptop", "Field/draft", "field/scratch"},
		{"laptop", "elsewhere", "lab/scratch"},
	}
	for _, c := range cases {
		f := newFixture(t, c.host)
		dir := f.path(c.dir)
		f.write(filepath.Join(dir, ".keep"), "")
		if got := strings.TrimSpace(f.ok(f.agent("namespace", dir)).stdout); got != c.want {
			t.Errorf("%s %s: got %s, want %s", c.host, c.dir, got, c.want)
		}
	}
	f := newFixture(t, "hub")
	f.registry["hosts"].(map[string]any)["hub"].(map[string]any)["meminiScratch"] = "shared"
	f.saveRegistry()
	if got := strings.TrimSpace(f.ok(f.agent("namespace", f.home)).stdout); got != "shared" {
		t.Errorf("configured scratch namespace: got %s", got)
	}
	f.write(".config/agents/projects.json", "invalid JSON")
	f.fails(f.agent("namespace", f.home), "invalid registry")
}
