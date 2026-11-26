// Package doctor lints this host's active project workspaces without writing anything: the notes
// heading contract, the mandatory context size, the recovery state, prose and the host's timers.
// Every check is one a script can decide and that breaks recovery when it fails, so the doctor
// stays quiet about taste.
package doctor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/janpuc/agent/internal/project"
	"github.com/janpuc/agent/internal/registry"
	"github.com/janpuc/agent/internal/workspace"
)

// warnPercent is where the mandatory context starts warning below its budget.
const warnPercent = 90

var (
	// projectHeadings and stateHeadings are the whole level-two contract of the two pages every
	// project carries, so an extra heading is a problem there.
	projectHeadings = []string{"Facts", "Operating constraints", "Systems", "Knowledge index"}
	stateHeadings   = []string{"Current work", "Verified state", "Open threads", "Pending approvals", "Next actions"}
	// topicHeadings is the opening structure of a topic page. A page may add sections after
	// History, the standard asks for these in order and not for exactly these.
	topicHeadings = []string{"Summary", "Evidence", "Verification", "History"}
	// timers are the units the host's recovery depends on.
	timers = []string{"agent-watch.timer", "agent-jobs.timer"}
)

// block is one group of findings, a project's or the host's.
type block struct {
	title string
	lines []string
}

// Run lints the active projects of this host and prints one block per project that has problems,
// then the host's own block, then a one line summary. A project name limits the pass to it. It
// returns the number of problems, which the command turns into its exit status.
func Run(h project.Host, only string, out io.Writer) (int, error) {
	names, err := active(h, only)
	if err != nil {
		return 0, err
	}
	mandatory := loadMandatory(h.Home)
	var blocks []block
	problems, projects := 0, 0
	for _, name := range names {
		lines := projectProblems(h, name, mandatory)
		if len(lines) == 0 {
			continue
		}
		blocks = append(blocks, block{title: name, lines: lines})
		problems += len(lines)
		projects++
	}
	if lines := hostProblems(h, mandatory); len(lines) > 0 {
		blocks = append(blocks, block{title: "host " + h.Name, lines: lines})
		problems += len(lines)
	}
	if problems == 0 {
		fmt.Fprintln(out, "ok")
		return 0, nil
	}
	for _, b := range blocks {
		fmt.Fprintln(out, b.title)
		for _, line := range b.lines {
			fmt.Fprintln(out, "  "+line)
		}
	}
	fmt.Fprintf(out, "%d %s in %d %s\n", problems, plural(problems, "problem"),
		projects, plural(projects, "project"))
	return problems, nil
}

// active lists the projects to lint: every active project of this host, or the one named.
func active(h project.Host, only string) ([]string, error) {
	if only != "" {
		lifecycle, err := h.Registry.Lifecycle(h.Home, only)
		if err != nil {
			return nil, err
		}
		if lifecycle != "active" {
			return nil, fmt.Errorf("%s: archived, nothing to lint", only)
		}
		return []string{only}, nil
	}
	var names []string
	for _, name := range h.Registry.Names(h.Name) {
		lifecycle, err := h.Registry.Lifecycle(h.Home, name)
		if err != nil {
			return nil, err
		}
		if lifecycle == "active" {
			names = append(names, name)
		}
	}
	return names, nil
}

// projectProblems runs the workspace checks over one active project.
func projectProblems(h project.Host, name string, m mandatory) []string {
	ws := filepath.Join(h.Root, name)
	if info, err := os.Stat(ws); err != nil || !info.IsDir() {
		return []string{"workspace missing"}
	}
	lines := notesProblems(ws)
	lines = append(lines, m.budgetProblems(ws)...)
	lines = append(lines, recoveryProblems(ws)...)
	return append(lines, proseProblems(ws)...)
}

// notesProblems checks the heading contract of the two pages every project carries and of each
// topic page, and reports a topic page that the knowledge index does not link.
func notesProblems(ws string) []string {
	lines := pageProblems(ws, "notes/PROJECT.md", projectHeadings, true)
	lines = append(lines, pageProblems(ws, "notes/STATE.md", stateHeadings, true)...)
	index, err := os.ReadFile(filepath.Join(ws, "notes/PROJECT.md"))
	if err != nil {
		// The missing page is already a line, and every topic would look unindexed.
		return lines
	}
	for _, path := range knowledgePages(ws) {
		rel := "notes/knowledge/" + filepath.Base(path)
		lines = append(lines, pageProblems(ws, rel, topicHeadings, false)...)
		if !bytes.Contains(index, []byte("knowledge/"+filepath.Base(path))) {
			lines = append(lines, rel+": not in the knowledge index")
		}
	}
	return lines
}

// knowledgePages lists a project's topic files, in name order.
func knowledgePages(ws string) []string {
	pages, _ := filepath.Glob(filepath.Join(ws, "notes/knowledge/*.md"))
	return pages
}

// pageProblems checks one page: exactly one title, then its level-two headings in order. extra
// reports a heading beyond the contract, which the two fixed pages forbid.
func pageProblems(ws, rel string, want []string, extra bool) []string {
	data, err := os.ReadFile(filepath.Join(ws, rel))
	if err != nil {
		if os.IsNotExist(err) {
			return []string{rel + ": missing"}
		}
		return []string{rel + ": cannot be read"}
	}
	titles := 0
	var got []string
	for _, heading := range headings(string(data)) {
		switch heading.level {
		case 1:
			titles++
		case 2:
			got = append(got, heading.text)
		}
	}
	if titles != 1 {
		return []string{fmt.Sprintf("%s: %d titles, want one", rel, titles)}
	}
	return headingProblems(rel, got, want, extra)
}

// headingProblems compares the level-two headings of a page with the ones it needs and returns
// only the first mismatch, so a page that lost its shape reads as one line, not as five.
func headingProblems(rel string, got, want []string, extra bool) []string {
	for i, heading := range want {
		switch {
		case i >= len(got):
			return []string{fmt.Sprintf("%s: missing heading %q", rel, heading)}
		case got[i] != heading:
			return []string{fmt.Sprintf("%s: heading %d is %q, want %q", rel, i+1, got[i], heading)}
		}
	}
	if extra && len(got) > len(want) {
		return []string{fmt.Sprintf("%s: extra heading %q", rel, got[len(want)])}
	}
	return nil
}

// heading is one markdown heading, one or two hashes deep, which is all the notes contract uses.
type heading struct {
	level int
	text  string
}

// headings reads the level-one and level-two headings of a page. A fenced block is skipped, so an
// example heading in code never counts.
func headings(data string) []heading {
	var out []heading
	fenced := false
	for _, line := range strings.Split(data, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		for level := 1; level <= 2; level++ {
			prefix := strings.Repeat("#", level) + " "
			if strings.HasPrefix(line, prefix) {
				out = append(out, heading{level: level, text: strings.TrimSpace(line[len(prefix):])})
				break
			}
		}
	}
	return out
}

// mandatory is the mandatory context the loader injects: the bytes of the standards files, the
// budget they share with a project's two pages, and whatever the manifest itself got wrong.
type mandatory struct {
	budget   int64
	bytes    int64
	problems []string
}

// manifest is the standards manifest, the one description of what the loader injects.
type manifest struct {
	Compatibility struct {
		BootstrapMaxBytes int64 `json:"bootstrapMaxBytes"`
	} `json:"compatibility"`
	Files []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

// loadMandatory reads the standards manifest, sizes and hashes every file it names, and keeps
// what is wrong with them for the host block.
func loadMandatory(home string) mandatory {
	dir := filepath.Join(home, ".config/agents/standards")
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return mandatory{problems: []string{"standards: manifest.json is missing"}}
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return mandatory{problems: []string{"standards: manifest.json is invalid"}}
	}
	result := mandatory{budget: m.Compatibility.BootstrapMaxBytes}
	for _, file := range m.Files {
		data, err := os.ReadFile(filepath.Join(dir, file.Path))
		if err != nil {
			result.problems = append(result.problems, "standards/"+file.Path+": missing")
			continue
		}
		result.bytes += sectionBytes("Standard: "+file.Path, len(data))
		sum := sha256.Sum256(data)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), file.SHA256) {
			result.problems = append(result.problems, "standards/"+file.Path+": sha256 differs from the manifest")
		}
	}
	return result
}

// sectionBytes is what the loader (project-context.ts) counts for one file: its text plus the
// heading and digest line it puts above it and the blank line between sections.
func sectionBytes(title string, size int) int64 {
	header := "## " + title + "\n\nsha256 " + strings.Repeat("0", 64) + ", " + strconv.Itoa(size) + " bytes.\n\n"
	return int64(len(header) + size + 2)
}

// budgetProblems checks what one project costs the loader: the standards, its PROJECT.md and its
// STATE.md against the manifest's budget, warning from ninety percent up.
func (m mandatory) budgetProblems(ws string) []string {
	if m.budget <= 0 {
		return nil
	}
	total := m.bytes
	for _, rel := range []string{"notes/PROJECT.md", "notes/STATE.md"} {
		if info, err := os.Stat(filepath.Join(ws, rel)); err == nil {
			total += sectionBytes(rel, int(info.Size()))
		}
	}
	percent := total * 100 / m.budget
	switch {
	case total > m.budget:
		return []string{fmt.Sprintf("context %d of %d bytes, %d%%, over budget", total, m.budget, percent)}
	case percent >= warnPercent:
		return []string{fmt.Sprintf("context %d of %d bytes, %d%%, near the budget", total, m.budget, percent)}
	}
	return nil
}

// recoveryProblems checks the two parts of a workspace Git never brings back: the untracked .agent
// state and the directories of the local-only members.
func recoveryProblems(ws string) []string {
	var lines []string
	state := filepath.Join(ws, ".agent")
	switch info, err := os.Stat(state); {
	case err != nil:
		lines = append(lines, "recovery: .agent is missing")
	case !info.IsDir():
		lines = append(lines, "recovery: .agent is not a directory")
	default:
		tracked, err := workspace.Git(ws, "ls-files", ".agent")
		switch {
		case err != nil:
			lines = append(lines, "recovery: git ls-files .agent failed")
		case tracked != "":
			files := strings.Count(tracked, "\n") + 1
			lines = append(lines, fmt.Sprintf("recovery: .agent has %d tracked %s", files, plural(files, "file")))
		}
	}
	members, err := workspace.Members(ws)
	if err != nil {
		return append(lines, "recovery: "+err.Error())
	}
	for _, member := range members {
		if !localOnly(member) {
			continue
		}
		path, err := workspace.MemberPath(ws, member.Path)
		if err != nil || !workspace.Exists(path) {
			lines = append(lines, "recovery: local-only member "+member.Path+": missing")
		}
	}
	return lines
}

// localOnly reports whether Git can never bring a member back. The agent marks one with a
// -local-only key and a local-only:// URL, and a bare agent-local-only URL is the older spelling.
func localOnly(member workspace.Member) bool {
	return member.LocalOnly || strings.HasPrefix(strings.ToLower(member.URL), "agent-local-only")
}

// proseProblems counts the lines of the top-level notes pages that carry an em dash, an en dash or
// an ellipsis, and names the first three. The rule is about punctuation, not about knowledge.
func proseProblems(ws string) []string {
	files, _ := filepath.Glob(filepath.Join(ws, "notes/*.md"))
	var found []string
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.ContainsAny(line, "\u2014\u2013\u2026") {
				found = append(found, fmt.Sprintf("%s:%d", filepath.Base(path), i+1))
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	shown := found
	if len(shown) > 3 {
		shown = shown[:3]
	}
	return []string{fmt.Sprintf("notes: %d %s with a dash or ellipsis, first at %s",
		len(found), plural(len(found), "line"), strings.Join(shown, ", "))}
}

// hostProblems checks the timers the watchdog and the job queue run on, then reports the state a
// slow command would: the newest watch log line and the newest backup state file. The backup
// status command reaches the NAS over SSH, so the doctor reads the timestamps instead. Those two
// lines are context for a problem, never a problem on their own.
func hostProblems(h project.Host, m mandatory) []string {
	lines := append([]string{}, m.problems...)
	for _, timer := range timers {
		state, err := timerState(timer)
		switch {
		case err != nil:
			lines = append(lines, timer+": systemctl is-active failed")
		case state != "active":
			lines = append(lines, timer+" is "+state)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	if watch := lastWatchLine(h.Home); watch != "" {
		lines = append(lines, "watch log last: "+watch)
	}
	if backup := newestBackupState(h); backup != "" {
		lines = append(lines, "backup state newest: "+backup)
	}
	return lines
}

// timerState asks systemd for one timer's state. is-active prints the state even when it exits
// non-zero, so its output is the answer and a missing systemctl is the only error.
func timerState(timer string) (string, error) {
	output, err := exec.Command("systemctl", "is-active", timer).Output()
	state := strings.TrimSpace(string(output))
	if state == "" {
		if err != nil {
			return "", err
		}
		return "unknown", nil
	}
	return state, nil
}

// lastWatchLine is the newest line of the watchdog log, which records what its timer last did.
func lastWatchLine(home string) string {
	data, err := os.ReadFile(filepath.Join(registry.StateDir(home), "watch.log"))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if lines[len(lines)-1] == "" {
		return ""
	}
	return lines[len(lines)-1]
}

// newestBackupState names the newest file of the host's backup state directory and when it was
// written, which shows restic's recency without running a status that waits on the NAS. A host
// whose registry entry names no backup script skips the check.
func newestBackupState(h project.Host) string {
	name := h.Registry.Hosts[h.Name].Backup
	if name == "" {
		return ""
	}
	entries, err := os.ReadDir(filepath.Join(h.Home, ".local/state", name))
	if err != nil {
		return ""
	}
	var newest os.DirEntry
	var when time.Time
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if newest == nil || info.ModTime().After(when) {
			newest, when = entry, info.ModTime()
		}
	}
	if newest == nil {
		return ""
	}
	return newest.Name() + " " + when.Local().Format("2006-01-02 15:04")
}

// plural adds an s when the count is not one.
func plural(count int, word string) string {
	if count == 1 {
		return word
	}
	return word + "s"
}
