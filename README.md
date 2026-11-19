# agent

Run one long-lived [Pi](https://github.com/badlogic/pi-mono) coding agent per project, each in
its own Rex terminal session, on one or more hosts. The `agent` command opens and
supervises project agents, carries requests and replies between them, runs short-lived tasks
next to a project and rotates an agent into a fresh session with a handoff when its context
grows old.

Status: early. The behavior is defined by the black-box test suite in `tests/`, which drives the
command line, files and external tools of an implementation. The Go implementation in
`cmd/agent` is being ported against that suite from an earlier Python runner.

## Concepts

- Registry: `~/.config/agents/projects.json` lists hosts, defaults and projects. Its `platform`
  block names the site's roles, so nothing site specific is compiled in.
- Workspace: `<host root>/<project>`, a Git repository with `notes/`, code repositories as
  submodules and runtime state in `.agent/`, which Git ignores.
- Generation: a project's Pi session. Rotation writes a handoff and moves to the next one.
- Requests: `agent ask <project> <text>` writes to that project's inbox, `agent reply` answers.
  A remote host reaches the hub through a restricted SSH forced command.
- Tasks: `agent task new` opens a task folder and a tab next to the project. Closing never
  deletes data.

## Registry

```json
{
  "platform": {
    "operator": "alice",
    "coordinator": "admin",
    "aliases": {},
    "clients": ["laptop"],
    "hub": "hub",
    "remote": {"project": "field", "host": "outpost", "identity": "~/.ssh/field-inbox"}
  },
  "hosts": {
    "hub": {"root": "~/Projects", "meminiPrefix": "lab", "guard": "personal"},
    "outpost": {"root": "~/Field", "meminiPrefix": "field", "guard": "work", "maxAgents": 5}
  },
  "defaults": {"host": "hub", "model": "provider/model", "thinking": "high", "tasks": {"maxOpen": 4}},
  "projects": {
    "admin": {},
    "garden": {"repos": [{"name": "code", "url": "https://example.com/code.git"}]},
    "field": {"host": "outpost"}
  }
}
```

## Tests

The suite needs Go and Git. Every other tool it meets (Rex, Pi, mise, ssh, chezmoi) is a stub.

```sh
go test ./tests                                              # builds ./cmd/agent
AGENT_IMPL=python AGENT_BIN=/path/to/agent-run go test -count=1 ./tests
```

## License

MIT
