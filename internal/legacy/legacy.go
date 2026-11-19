// Package legacy hands commands that are not ported yet to the Python runner. It goes away when
// the port is complete.
package legacy

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Path is AGENT_LEGACY or ~/.local/bin/agent-run.
func Path(home string) string {
	if path := os.Getenv("AGENT_LEGACY"); path != "" {
		return path
	}
	return filepath.Join(home, ".local/bin/agent-run")
}

// Exec replaces this process with the legacy runner and its arguments.
func Exec(home string, args []string) error {
	path := Path(home)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("command not ported yet and no legacy runner at %s", path)
	}
	argv := append([]string{path}, args...)
	return syscall.Exec(path, argv, os.Environ())
}
