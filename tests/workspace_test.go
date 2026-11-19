package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bareRemote makes a repository with one commit and a bare clone to use as its remote.
func (f *fixture) bareRemote() (source, remote string) {
	source, remote = filepath.Join(f.dir, "source"), filepath.Join(f.dir, "remote.git")
	f.git(f.dir, "init", "--initial-branch=main", source)
	f.write(filepath.Join(source, "file.txt"), "first\n")
	f.git(source, "add", "--", "file.txt")
	f.git(source, "commit", "-m", "First commit")
	f.git(f.dir, "clone", "--bare", "--", source, remote)
	return source, remote
}

func TestOpenInitializesWorkspace(t *testing.T) {
	f := newFixture(t, "hub")
	garden := f.open("garden")
	for _, path := range []string{"notes/PROJECT.md", "notes/STATE.md", "mise.toml", ".gitignore"} {
		if !f.exists(filepath.Join(garden, path)) {
			t.Fatalf("missing %s", path)
		}
	}
	if f.git(garden, "status", "--porcelain") != "" || f.git(garden, "log", "--format=%s") != "Initialize project workspace" {
		t.Fatal("workspace not committed cleanly")
	}
	if !strings.Contains(f.read(filepath.Join(garden, ".gitignore")), ".agent/") {
		t.Fatal(".agent is not ignored")
	}
	record := readJSON[map[string]any](t, f.path(".local/state/agents/initialized/garden"))
	if record["schemaVersion"] != 1.0 || record["project"] != "garden" || record["workspace"] != garden ||
		record["origin"] != "bootstrap" || record["state"] != "initialized" {
		t.Fatalf("initialization record %v", record)
	}
}

func TestRegisterLocalOnlyAndMissingMemberNeedsRestore(t *testing.T) {
	f := newFixture(t, "hub")
	garden := f.open("garden")
	f.ok(f.internal("--register", "garden", "local"))
	modules := filepath.Join(garden, ".gitmodules")
	// The marker key is agent-local-only. Workspaces from the Python runner carry a site-named
	// key ending in -local-only, which implementations keep reading.
	marker := regexp.MustCompile(`(?m)^\s*[a-z]+-local-only = true$`)
	if f.git(garden, "config", "--file", modules, "submodule.local.url") != "local-only://garden/local" ||
		!marker.MatchString(f.read(modules)) {
		t.Fatalf(".gitmodules\n%s", f.read(modules))
	}
	f.git(garden, "commit", "-m", "Register local member")
	head := f.git(filepath.Join(garden, "local"), "rev-parse", "HEAD")
	if !strings.Contains(f.git(garden, "ls-tree", "HEAD", "--", "local"), "160000 commit "+head) {
		t.Fatal("gitlink does not record the member's HEAD")
	}
	f.fails(f.internal("--register", "garden", "local"), "already registered")
	f.fails(f.internal("--register", "garden", "notes"), "reserved")
	f.fails(f.internal("--register", "garden", "../escape"), "Invalid")
	must(t, os.RemoveAll(filepath.Join(garden, "local")))
	f.fails(f.agent("garden"), "restore needed")
	if f.exists(filepath.Join(garden, "local")) {
		t.Fatal("a missing local-only member was recreated empty")
	}
}

func TestMissingRemoteMemberRestoredAtRecordedCommit(t *testing.T) {
	f := newFixture(t, "hub")
	source, remote := f.bareRemote()
	f.project("garden")["repos"] = []any{
		map[string]any{"name": "code", "url": remote}, map[string]any{"name": "other", "url": remote}}
	f.saveRegistry()
	garden := f.open("garden")
	original := f.git(filepath.Join(garden, "code"), "rev-parse", "HEAD")
	other := filepath.Join(garden, "other")
	f.git(other, "checkout", "--detach", "HEAD")
	f.write(filepath.Join(other, "file.txt"), "keep other work\n")
	before := f.git(other, "status", "--porcelain")
	f.write(filepath.Join(source, "file.txt"), "new remote head\n")
	f.git(source, "commit", "-am", "Advance remote")
	f.git(source, "push", remote, "main")
	must(t, os.RemoveAll(filepath.Join(garden, "code")))
	f.open("garden")
	code := filepath.Join(garden, "code")
	if f.git(code, "rev-parse", "HEAD") != original || f.git(code, "branch", "--show-current") != "agent-restored" ||
		f.read(filepath.Join(code, "file.txt")) != "first\n" {
		t.Fatal("member not restored at the recorded commit on agent-restored")
	}
	if f.git(other, "status", "--porcelain") != before || f.git(other, "rev-parse", "HEAD") != original ||
		f.git(other, "branch", "--show-current") != "" || f.read(filepath.Join(other, "file.txt")) != "keep other work\n" {
		t.Fatal("restore touched another member")
	}
	if listing := f.ok(f.agent("ls")).stdout; !strings.Contains(listing, "other:branch=detached,dirty=yes") {
		t.Fatalf("listing\n%s", listing)
	}
}

func TestDirtyAndDetachedMembersUntouched(t *testing.T) {
	f := newFixture(t, "hub")
	_, remote := f.bareRemote()
	f.project("garden")["repos"] = []any{map[string]any{"name": "code", "url": remote}}
	f.saveRegistry()
	code := filepath.Join(f.open("garden"), "code")
	original := f.git(code, "rev-parse", "HEAD")
	f.git(code, "checkout", "--detach", original)
	f.write(filepath.Join(code, "file.txt"), "uncommitted\n")
	f.write(filepath.Join(code, "untracked.txt"), "keep me\n")
	before := f.git(code, "status", "--porcelain")
	f.open("garden")
	if f.git(code, "status", "--porcelain") != before || f.git(code, "rev-parse", "HEAD") != original ||
		f.git(code, "branch", "--show-current") != "" || f.read(filepath.Join(code, "file.txt")) != "uncommitted\n" ||
		f.read(filepath.Join(code, "untracked.txt")) != "keep me\n" {
		t.Fatal("open changed a dirty detached member")
	}
}

func TestMissingInitializedAndUnadoptedWorkspacesRefuseBootstrap(t *testing.T) {
	f := newFixture(t, "hub")
	garden := f.open("garden")
	must(t, os.RemoveAll(garden))
	f.fails(f.agent("garden"), "restore needed")
	if f.exists(garden) || !f.exists(".local/state/agents/initialized/garden") {
		t.Fatal("a lost workspace was recreated or its record dropped")
	}
	admin := f.workspace("admin")
	f.git(f.dir, "init", "--initial-branch=main", admin)
	f.write(filepath.Join(admin, "untouched.txt"), "legacy data\n")
	f.fails(f.agent("admin"), "adoption required")
	if f.read(filepath.Join(admin, "untouched.txt")) != "legacy data\n" || f.exists(".local/state/agents/initialized/admin") {
		t.Fatal("an unadopted workspace was changed")
	}
	f.git(admin, "add", "untouched.txt")
	f.git(admin, "commit", "-m", "Legacy")
	f.ok(f.agent("adopt", "admin"))
	if readJSON[map[string]any](t, f.path(".local/state/agents/initialized/admin"))["origin"] != "adopted" {
		t.Fatal("adopt did not record the workspace")
	}
	f.fails(f.agent("adopt", "admin"), "already has an initialization record")
	f.open("admin")
}
