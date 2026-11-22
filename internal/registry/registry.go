// Package registry reads ~/.config/agents/projects.json, the site's hosts, defaults and projects.
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Registry is the effective registry. Unknown fields are kept in the raw maps.
type Registry struct {
	Platform Platform           `json:"platform"`
	Hosts    map[string]Host    `json:"hosts"`
	Defaults Defaults           `json:"defaults"`
	Projects map[string]Project `json:"projects"`
}

// Platform names the site's roles, so no site is compiled in.
type Platform struct {
	// Operator is the reserved sender for requests from a laptop client.
	Operator string `json:"operator"`
	// Coordinator receives the operator's replies.
	Coordinator string `json:"coordinator"`
	// Aliases map senders of requests made before a project rename.
	Aliases map[string]string `json:"aliases"`
	// Clients are dispatch-only machines.
	Clients []string `json:"clients"`
	// Hub receives remote deliveries and queues replies for the remote project.
	Hub    string `json:"hub"`
	Remote Remote `json:"remote"`
	// Tickets is the project that opens a task for a bare ticket id outside any project.
	Tickets string `json:"tickets"`
	// Workers are machines that run jobs over SSH when they are switched on, such as a laptop.
	// The SSH config names each one, authorized_keys on the worker forces --worker-endpoint.
	Workers []string `json:"workers"`
}

// Remote is the one project on another host that talks to the hub over SSH.
type Remote struct {
	Project  string `json:"project"`
	Host     string `json:"host"`
	Identity string `json:"identity"`
}

// Host is one execution host.
type Host struct {
	Root          string `json:"root"`
	MeminiPrefix  string `json:"meminiPrefix"`
	MeminiScratch string `json:"meminiScratch"`
	MeminiHome    string `json:"meminiHome"`
	Guard         string `json:"guard"`
	MaxAgents     *int   `json:"maxAgents"`
	Providers     Policy `json:"providers"`
	// ScratchProject is the project that holds free-standing work on this host.
	ScratchProject string `json:"scratchProject"`
}

type Defaults struct {
	Host         string         `json:"host"`
	Model        string         `json:"model"`
	Thinking     string         `json:"thinking"`
	MeminiPrefix string         `json:"meminiPrefix"`
	Providers    Policy         `json:"providers"`
	Tasks        map[string]int `json:"tasks"`
}

type Project struct {
	Host            string            `json:"host"`
	Lifecycle       string            `json:"lifecycle"`
	MeminiNamespace string            `json:"meminiNamespace"`
	Model           string            `json:"model"`
	Thinking        string            `json:"thinking"`
	Providers       Policy            `json:"providers"`
	Secrets         map[string]string `json:"secrets"`
	Tasks           map[string]int    `json:"tasks"`
	Repos           []Repo            `json:"repos"`
}

type Repo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ValidName reports whether name is a single safe path component.
func ValidName(name string) bool { return validName.MatchString(name) }

// Path is the registry location under home.
func Path(home string) string { return filepath.Join(home, ".config/agents/projects.json") }

// Load reads and checks the registry.
func Load(home string) (*Registry, error) {
	data, err := os.ReadFile(Path(home))
	if err != nil {
		return nil, err
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("invalid registry %s: %w", Path(home), err)
	}
	if r.Projects == nil || r.Hosts == nil {
		return nil, fmt.Errorf("invalid registry %s: hosts and projects are required", Path(home))
	}
	return &r, nil
}

// Operator is the reserved sender for requests from a laptop client.
func (r *Registry) Operator() (string, error) {
	if r.Platform.Operator == "" {
		return "", fmt.Errorf("registry platform is missing operator")
	}
	return r.Platform.Operator, nil
}

// Coordinator receives the operator's replies.
func (r *Registry) Coordinator() (string, error) {
	if r.Platform.Coordinator == "" {
		return "", fmt.Errorf("registry platform is missing coordinator")
	}
	return r.Platform.Coordinator, nil
}

// Hub receives remote deliveries and queues replies for the remote project.
func (r *Registry) HubSite() (string, error) {
	if r.Platform.Hub == "" {
		return "", fmt.Errorf("registry platform is missing hub")
	}
	return r.Platform.Hub, nil
}

// RemoteSite is the project on another host that talks to the hub over SSH.
func (r *Registry) RemoteSite() (Remote, error) {
	remote := r.Platform.Remote
	switch {
	case remote.Project == "":
		return Remote{}, fmt.Errorf("registry platform is missing remote.project")
	case remote.Host == "":
		return Remote{}, fmt.Errorf("registry platform is missing remote.host")
	case remote.Identity == "":
		return Remote{}, fmt.Errorf("registry platform is missing remote.identity")
	}
	return remote, nil
}

// IsClient reports whether host is a dispatch-only client.
func (r *Registry) IsClient(host string) bool {
	for _, client := range r.Platform.Clients {
		if client == host {
			return true
		}
	}
	return false
}

// HostName is this machine's short lowercase name. AGENT_HOST overrides it for tests, which play
// every host role on one machine.
func HostName() (string, error) {
	name := os.Getenv("AGENT_HOST")
	if name == "" {
		var err error
		if name, err = os.Hostname(); err != nil {
			return "", err
		}
	}
	name, _, _ = strings.Cut(name, ".")
	return strings.ToLower(name), nil
}

// ProjectHost is the host a project runs on: its own, the default, or the hub.
func (r *Registry) ProjectHost(name string) string {
	if host := r.Projects[name].Host; host != "" {
		return host
	}
	if r.Defaults.Host != "" {
		return r.Defaults.Host
	}
	return r.Platform.Hub
}

// Policy is a providers policy that remembers whether the registry set it, so an explicit null
// is rejected instead of being mistaken for an absent policy.
type Policy struct {
	Set   bool
	Value any
}

func (p *Policy) UnmarshalJSON(data []byte) error {
	p.Set = true
	return json.Unmarshal(data, &p.Value)
}

// Names lists projects on host in name order, or every project when host is empty.
func (r *Registry) Names(host string) []string {
	var names []string
	for name := range r.Projects {
		if host == "" || r.ProjectHost(name) == host {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Root is a host's absolute, symlink-resolved workspace root.
func (r *Registry) Root(home, host string) (string, error) {
	h, ok := r.Hosts[host]
	if !ok || h.Root == "" {
		return "", fmt.Errorf("host %s has no root in the registry", host)
	}
	root := h.Root
	if root == "~" || strings.HasPrefix(root, "~/") {
		root = filepath.Join(home, strings.TrimPrefix(root, "~"))
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return filepath.Abs(root)
}

// StateDir holds platform records such as initialization and lifecycle.
func StateDir(home string) string { return filepath.Join(home, ".local/state/agents") }

// Lifecycle is active or archived. A platform record overrides the registry.
func (r *Registry) Lifecycle(home, name string) (string, error) {
	value := r.Projects[name].Lifecycle
	if data, err := os.ReadFile(filepath.Join(StateDir(home), "lifecycle", name)); err == nil {
		value = strings.TrimSpace(string(data))
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if value == "" {
		value = "active"
	}
	if value != "active" && value != "archived" {
		return "", fmt.Errorf("%s: invalid lifecycle %s", name, value)
	}
	return value, nil
}

// Namespace is the memini namespace of a registered project.
func (r *Registry) Namespace(name string) string {
	if ns := r.Projects[name].MeminiNamespace; ns != "" {
		return ns
	}
	prefix := r.Hosts[r.ProjectHost(name)].MeminiPrefix
	if prefix == "" {
		prefix = r.Defaults.MeminiPrefix
	}
	return prefix + "/" + name
}

// ScratchProject is the project that holds this host's scratch work.
func (r *Registry) ScratchProject(host string) (string, error) {
	name := r.Hosts[host].ScratchProject
	if name == "" {
		return "", fmt.Errorf("%s: no scratch project configured", host)
	}
	return name, nil
}

// Scratch is the shared namespace for sessions outside registered projects on host.
func (r *Registry) Scratch(host string) string {
	if ns := r.Hosts[host].MeminiScratch; ns != "" {
		return ns
	}
	prefix := r.Hosts[host].MeminiPrefix
	if prefix == "" {
		prefix = r.Defaults.MeminiPrefix
	}
	return prefix + "/scratch"
}
