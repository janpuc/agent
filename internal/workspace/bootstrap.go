package workspace

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/janpuc/agent/internal/registry"
)

// statePage is the initial STATE.md of a project and of every task.
const statePage = "# State\n\n## Current work\n\n## Verified state\n\n## Open threads\n\n## Pending approvals\n\n## Next actions\n"

// Bootstrap returns the project's workspace, creating it from the registry's seed repositories
// when it was never initialized. An existing workspace is only inspected.
func Bootstrap(reg *registry.Registry, home, root, name string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	folder := filepath.Join(root, name)
	record, err := Initialization(home, root, name)
	if err != nil {
		return "", err
	}
	if record != nil {
		if record.State != "initialized" || !IsDir(folder) {
			return "", fmt.Errorf("%s: restore needed, previously initialized workspace is missing or incomplete", name)
		}
		if err := RequireGitRoot(folder); err != nil {
			return "", err
		}
		return folder, nil
	}
	if Exists(folder) || IsSymlink(folder) {
		return "", fmt.Errorf("%s: adoption required: run agent adopt %s", name, name)
	}
	project := reg.Projects[name]
	names := map[string]bool{}
	for _, repo := range project.Repos {
		if names[repo.Name] {
			return "", fmt.Errorf("%s: duplicate seed repository", name)
		}
		names[repo.Name] = true
	}
	for _, repo := range project.Repos {
		if _, err := MemberPath(folder, repo.Name); err != nil {
			return "", err
		}
	}
	if err := RecordInitialization(home, root, name, "bootstrap", "initializing"); err != nil {
		return "", err
	}
	err = initialize(home, root, name, folder, project)
	if err == nil {
		err = RecordInitialization(home, root, name, "bootstrap", "initialized")
	}
	if err != nil {
		// If installation completed, keep the initializing record and require recovery.
		if !Exists(folder) {
			os.Remove(InitializationPath(home, name))
		}
		return "", err
	}
	return folder, nil
}

// initialize builds the workspace in a temporary directory next to the root and renames it into
// place only when the target still does not exist.
func initialize(home, root, name, folder string, project registry.Project) error {
	temp, err := os.MkdirTemp(root, ".agent-init-"+name+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	checkout := filepath.Join(temp, name)
	if _, err := Git(root, "init", "--initial-branch=main", checkout); err != nil {
		return err
	}
	for _, path := range []string{"notes/knowledge", "plans", "scripts", ".agent", ".worktrees"} {
		if err := os.MkdirAll(filepath.Join(checkout, path), 0o755); err != nil {
			return err
		}
	}
	mise := "# Project tools, tasks and non-secret environment\n"
	if len(project.Secrets) > 0 {
		mise = fmt.Sprintf("[env]\n_.file = \"%s\"\n", filepath.Join(home, ".local/state/ai/projects", name+".env"))
	}
	files := map[string]string{
		".gitignore":       ".agent/\n.worktrees/\n",
		"notes/PROJECT.md": "# " + name + "\n\n## Facts\n\n## Operating constraints\n\n## Systems\n\n## Knowledge index\n",
		"notes/STATE.md":   statePage,
		"mise.toml":        mise,
	}
	for path, text := range files {
		if err := os.WriteFile(filepath.Join(checkout, path), []byte(text), 0o644); err != nil {
			return err
		}
	}
	if _, err := Git(checkout, "add", "--", ".gitignore", "notes/PROJECT.md", "notes/STATE.md", "mise.toml"); err != nil {
		return err
	}
	for _, repo := range project.Repos {
		if err := RegisterRepository(checkout, name, repo.Name, repo.URL); err != nil {
			return err
		}
	}
	if _, err := Git(checkout, "commit", "-m", "Initialize project workspace"); err != nil {
		return err
	}
	if Exists(folder) || IsSymlink(folder) {
		return fmt.Errorf("%s: workspace appeared during initialization", name)
	}
	return os.Rename(checkout, folder)
}
