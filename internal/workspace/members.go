package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/janpuc/agent/internal/registry"
)

// Member is one repository of a workspace.
type Member struct {
	Name string
	Path string
	URL  string
	// LocalOnly is a member that was created inside the workspace and has no remote.
	LocalOnly bool
	// Unregistered is a repository found in the workspace but missing from .gitmodules.
	Unregistered bool
}

// reservedMembers are workspace directories that can never be member repositories.
var reservedMembers = map[string]bool{"notes": true, "plans": true, "scripts": true, "tasks": true}

// submoduleKey is one key of .gitmodules. Older workspaces mark a local-only member with a site
// named key, so any key ending in -local-only carries the marker.
var submoduleKey = regexp.MustCompile(`^submodule\.(.+)\.(path|url|.+-local-only)$`)

// Git runs one git command in a repository and returns its trimmed standard output.
func Git(folder string, args ...string) (string, error) {
	output, err := exec.Command("git", append([]string{"-C", folder}, args...)...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// MemberPath is the directory of a member in the workspace.
func MemberPath(folder, name string) (string, error) {
	if !registry.ValidName(name) || reservedMembers[name] {
		return "", fmt.Errorf("Invalid or reserved member name: %s", name)
	}
	target := filepath.Join(folder, name)
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("Member path escapes workspace: %s", target)
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil && filepath.Dir(resolved) != folder {
		return "", fmt.Errorf("Member path escapes workspace: %s", target)
	}
	return target, nil
}

// RequireGitRoot checks that path is a directory and the root of its own repository.
func RequireGitRoot(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("Not a workspace directory: %s", path)
	}
	top, err := Git(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		top = resolved
	}
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if top != root {
		return fmt.Errorf("Not a Git root: %s", path)
	}
	return nil
}

// Members lists the repositories .gitmodules registers, in file order.
func Members(folder string) ([]Member, error) {
	path := filepath.Join(folder, ".gitmodules")
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("Symlinked membership file: %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	output, err := exec.Command("git", "config", "--null", "--file", path, "--list").Output()
	if err != nil {
		return nil, err
	}
	var order []string
	entries := map[string]map[string]string{}
	for _, entry := range strings.Split(string(output), "\x00") {
		if entry == "" {
			continue
		}
		key, value, _ := strings.Cut(entry, "\n")
		match := submoduleKey.FindStringSubmatch(key)
		if match == nil {
			continue
		}
		name, field := match[1], match[2]
		values, ok := entries[name]
		if !ok {
			values = map[string]string{}
			entries[name] = values
			order = append(order, name)
		}
		if _, duplicate := values[field]; duplicate {
			return nil, fmt.Errorf("Duplicate submodule field: %s", key)
		}
		values[field] = value
	}
	paths := map[string]bool{}
	members := make([]Member, 0, len(order))
	for _, name := range order {
		values := entries[name]
		path, registered := values["path"]
		if !registry.ValidName(name) || !registered || values["url"] == "" {
			return nil, fmt.Errorf("Invalid submodule metadata: %s", name)
		}
		if _, err := MemberPath(folder, path); err != nil {
			return nil, err
		}
		if paths[path] {
			return nil, fmt.Errorf("Duplicate member path: %s", path)
		}
		paths[path] = true
		marker := "false"
		for field, value := range values {
			if strings.HasSuffix(field, "-local-only") {
				marker = value
			}
		}
		if marker != "true" && marker != "false" {
			return nil, fmt.Errorf("Invalid local-only marker: %s", name)
		}
		localOnly := marker == "true"
		if strings.HasPrefix(values["url"], "local-only://") && !localOnly {
			return nil, fmt.Errorf("Missing local-only marker: %s", name)
		}
		members = append(members, Member{Name: name, Path: path, URL: values["url"], LocalOnly: localOnly})
	}
	return members, nil
}

// recordedCommit is the commit a workspace's gitlink records for a member.
func recordedCommit(folder string, member Member) (string, error) {
	output, err := Git(folder, "ls-tree", "HEAD", "--", member.Path)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	if len(fields) < 3 || fields[0] != "160000" || fields[1] != "commit" {
		return "", fmt.Errorf("%s: no committed gitlink, checkpoint the workspace first", member.Name)
	}
	return fields[2], nil
}

// RestoreMember checks an existing member or clones a missing remote member at the commit the
// workspace records. A local-only member is data that only a backup can bring back.
func RestoreMember(folder string, member Member) error {
	target, err := MemberPath(folder, member.Path)
	if err != nil {
		return err
	}
	if Exists(target) {
		return RequireGitRoot(target)
	}
	if member.LocalOnly {
		return fmt.Errorf("%s: local-only member missing, restore needed from backup", member.Name)
	}
	commit, err := recordedCommit(folder, member)
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(folder), ".agent-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	checkout := filepath.Join(temp, "repo")
	if _, err := Git(folder, "clone", "--no-checkout", "--", member.URL, checkout); err != nil {
		return err
	}
	if _, err := Git(checkout, "cat-file", "-e", commit+"^{commit}"); err != nil {
		if _, err := Git(checkout, "fetch", "origin", commit); err != nil {
			return err
		}
	}
	// This is a disposable clone, including if its default branch has this name.
	if _, err := Git(checkout, "checkout", "-B", "agent-restored", commit); err != nil {
		return err
	}
	if Exists(target) || IsSymlink(target) {
		return fmt.Errorf("Member appeared during recovery, leaving it alone: %s", target)
	}
	return os.Rename(checkout, target)
}

// RegisterRepository registers and stages a member. The caller commits the workspace checkpoint.
func RegisterRepository(folder, project, name, url string) error {
	if err := RequireGitRoot(folder); err != nil {
		return err
	}
	target, err := MemberPath(folder, name)
	if err != nil {
		return err
	}
	registered, err := Members(folder)
	if err != nil {
		return err
	}
	for _, member := range registered {
		if member.Name == name || member.Path == name {
			return fmt.Errorf("Member already registered: %s", name)
		}
	}
	if !Exists(target) {
		temp, err := os.MkdirTemp(filepath.Dir(folder), ".agent-register-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(temp)
		checkout := filepath.Join(temp, "repo")
		if url != "" {
			if _, err := Git(folder, "clone", "--", url, checkout); err != nil {
				return err
			}
		} else {
			if _, err := Git(folder, "init", "--initial-branch=main", checkout); err != nil {
				return err
			}
			if _, err := Git(checkout, "commit", "--allow-empty", "-m", "Initialize local repository"); err != nil {
				return err
			}
		}
		if Exists(target) || IsSymlink(target) {
			return fmt.Errorf("Member appeared during registration: %s", target)
		}
		if err := os.Rename(checkout, target); err != nil {
			return err
		}
	}
	if err := RequireGitRoot(target); err != nil {
		return err
	}
	if _, err := Git(target, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return err
	}
	modules := filepath.Join(folder, ".gitmodules")
	if _, err := Git(folder, "config", "--file", modules, "submodule."+name+".path", name); err != nil {
		return err
	}
	link := url
	if link == "" {
		link = "local-only://" + project + "/" + name
	}
	if _, err := Git(folder, "config", "--file", modules, "submodule."+name+".url", link); err != nil {
		return err
	}
	if url == "" {
		// The marker key names no site, so a workspace stays portable between them.
		if _, err := Git(folder, "config", "--file", modules, "submodule."+name+".agent-local-only", "true"); err != nil {
			return err
		}
	}
	if _, err := Git(folder, "-c", "advice.addEmbeddedRepo=false", "add", "--", ".gitmodules", name); err != nil {
		return err
	}
	_, err = Git(folder, "submodule", "init", "--", name)
	return err
}

// RepositoryStatus describes every repository in the workspace, one line each.
func RepositoryStatus(folder string) ([]string, error) {
	registered, err := Members(folder)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, member := range registered {
		known[member.Path] = true
	}
	// Inspection also shows repositories that were never registered as members.
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !registry.ValidName(name) || known[name] || !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(folder, name, ".git")); err != nil {
			continue
		}
		registered = append(registered, Member{Name: name, Path: name, Unregistered: true})
	}
	var statuses []string
	for _, member := range registered {
		target, err := MemberPath(folder, member.Path)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(target); err != nil {
			suffix := ""
			if member.LocalOnly {
				suffix = "-local-only"
			}
			statuses = append(statuses, fmt.Sprintf("%s:missing%s", member.Name, suffix))
			continue
		}
		if err := RequireGitRoot(target); err != nil {
			return nil, err
		}
		dirty, err := Git(target, "status", "--porcelain")
		if err != nil {
			return nil, err
		}
		branch, ahead := "detached", "unknown"
		if name, err := Git(target, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
			branch = name
		}
		if _, err := Git(target, "rev-parse", "--verify", "@{upstream}"); err == nil {
			if ahead, err = Git(target, "rev-list", "--count", "@{upstream}..HEAD"); err != nil {
				return nil, err
			}
		}
		label, clean := member.Name+":", "no"
		if member.Unregistered {
			label += "unregistered,"
		}
		if dirty != "" {
			clean = "yes"
		}
		statuses = append(statuses, fmt.Sprintf("%sbranch=%s,dirty=%s,ahead=%s", label, branch, clean, ahead))
	}
	return statuses, nil
}
