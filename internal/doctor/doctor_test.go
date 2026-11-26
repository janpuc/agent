package doctor

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// page writes one notes page under a temporary workspace.
func page(t *testing.T, ws, rel, text string) {
	t.Helper()
	path := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHeadingProblemsReportsTheFirstMismatch covers check one and two without touching a host:
// the ordered headings, the extra heading a fixed page forbids, and the section a topic page may
// add after History.
func TestHeadingProblemsReportsTheFirstMismatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rel   string
		got   []string
		want  []string
		extra bool
		lines []string
	}{
		{name: "in order", rel: "notes/PROJECT.md", got: []string{"Facts", "Operating constraints",
			"Systems", "Knowledge index"}, want: projectHeadings, extra: true},
		{name: "swapped", rel: "notes/PROJECT.md", got: []string{"Facts", "Systems",
			"Operating constraints", "Knowledge index"}, want: projectHeadings, extra: true,
			lines: []string{`notes/PROJECT.md: heading 2 is "Systems", want "Operating constraints"`}},
		{name: "extra on a fixed page", rel: "notes/PROJECT.md", got: []string{"Facts",
			"Operating constraints", "Systems", "Knowledge index", "History"}, want: projectHeadings,
			extra: true, lines: []string{`notes/PROJECT.md: extra heading "History"`}},
		{name: "truncated", rel: "notes/STATE.md", got: []string{"Current work", "Verified state",
			"Open threads"}, want: stateHeadings, extra: true,
			lines: []string{`notes/STATE.md: missing heading "Pending approvals"`}},
		{name: "topic section after History", rel: "notes/knowledge/topic.md", got: []string{"Summary",
			"Evidence", "Verification", "History", "Timeline"}, want: topicHeadings},
		{name: "topic out of order", rel: "notes/knowledge/topic.md", got: []string{"Summary",
			"History", "Evidence", "Verification"}, want: topicHeadings,
			lines: []string{`notes/knowledge/topic.md: heading 2 is "History", want "Evidence"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := headingProblems(tc.rel, tc.got, tc.want, tc.extra)
			if strings.Join(got, "\n") != strings.Join(tc.lines, "\n") {
				t.Fatalf("problems %q, want %q", got, tc.lines)
			}
		})
	}
}

// TestPageProblemsWantsOneTitle covers the page check around the headings: a title, and a file
// that is not there.
func TestPageProblemsWantsOneTitle(t *testing.T) {
	ws := t.TempDir()
	page(t, ws, "notes/PROJECT.md", "# garden\n\n# also garden\n\n## Facts\n\n## Operating constraints\n\n## Systems\n\n## Knowledge index\n")
	page(t, ws, "notes/STATE.md", "## Current work\n")
	if got := pageProblems(ws, "notes/PROJECT.md", projectHeadings, true); strings.Join(got, "|") !=
		`notes/PROJECT.md: 2 titles, want one` {
		t.Fatalf("two titles: %q", got)
	}
	if got := pageProblems(ws, "notes/STATE.md", stateHeadings, true); strings.Join(got, "|") !=
		`notes/STATE.md: 0 titles, want one` {
		t.Fatalf("no title: %q", got)
	}
	if got := pageProblems(ws, "notes/GONE.md", stateHeadings, true); strings.Join(got, "|") !=
		"notes/GONE.md: missing" {
		t.Fatalf("missing page: %q", got)
	}
}

// TestBudgetProblemsWarnsAtNinetyPercent covers check three: the standards, PROJECT.md and
// STATE.md share one budget, which warns at ninety percent and only fails above it. The two pages
// of the workspace below are 100 bytes each, with the loader's section headers on top, so the
// standards bytes are chosen to reach each total.
func TestBudgetProblemsWarnsAtNinetyPercent(t *testing.T) {
	ws := t.TempDir()
	page(t, ws, "notes/PROJECT.md", strings.Repeat("p", 100))
	page(t, ws, "notes/STATE.md", strings.Repeat("s", 100))
	pages := sectionBytes("notes/PROJECT.md", 100) + sectionBytes("notes/STATE.md", 100)
	for _, tc := range []struct {
		name  string
		total int64
		lines []string
	}{
		{name: "under", total: 800},
		{name: "at ninety percent", total: 900,
			lines: []string{"context 900 of 1000 bytes, 90%, near the budget"}},
		{name: "at the budget", total: 1000,
			lines: []string{"context 1000 of 1000 bytes, 100%, near the budget"}},
		{name: "over", total: 1050,
			lines: []string{"context 1050 of 1000 bytes, 105%, over budget"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mandatory{bytes: tc.total - pages + 0, budget: 1000}.budgetProblems(ws)
			if strings.Join(got, "|") != strings.Join(tc.lines, "|") {
				t.Fatalf("problems %q, want %q", got, tc.lines)
			}
		})
	}
}

// TestLoadContextChecksTheStandardsHashes covers the manifest half of check three: a standards
// file that drifted from the hash the manifest records is a finding, and so is a missing file.
func TestLoadContextChecksTheStandardsHashes(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config/agents/standards")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "knowledge\n"
	sum := sha256.Sum256([]byte(text))
	manifest := fmt.Sprintf(`{"compatibility":{"bootstrapMaxBytes":49152},"files":[`+
		`{"path":"knowledge.md","sha256":"%x"},{"path":"gone.md","sha256":"00"}]}`, sum)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "knowledge.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	mandatory := loadMandatory(home)
	if mandatory.budget != 49152 || mandatory.bytes != sectionBytes("Standard: knowledge.md", len(text)) {
		t.Fatalf("mandatory %+v", mandatory)
	}
	if strings.Join(mandatory.problems, "|") != "standards/gone.md: missing" {
		t.Fatalf("problems %q", mandatory.problems)
	}
	if err := os.WriteFile(filepath.Join(dir, "knowledge.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mandatory = loadMandatory(home)
	if !strings.Contains(strings.Join(mandatory.problems, "|"), "standards/knowledge.md: sha256 differs from the manifest") {
		t.Fatalf("drifted standards %q", mandatory.problems)
	}
}
