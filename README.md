# agent

Run one long-lived [Pi](https://github.com/badlogic/pi-mono) coding agent per project, each in
its own Rex terminal session, on one or more hosts. The `agent` command opens and
supervises project agents, carries requests and replies between them, runs short-lived tasks
next to a project and rotates an agent into a fresh session with a handoff when its context
grows old.

Status: early. The behavior is defined by the black-box test suite in `tests/`, which drives the
command line, files and external tools of an implementation. `cmd/agent` is the reference
implementation; the suite also runs against the earlier Python runner while it is kept.

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
- Watchdog: `agent watch` is the one pass the host's timer runs, outside Rex, so a Rex restart
  never takes it down. It starts a project whose runner died and continues a session whose last
  turn ended on a provider stall.
- Scratch: `agent scratch` keeps free-standing work as tasks of the project each host names in
  `scratchProject`. A scratch task folds into a project or asks the coordinator to register a new
  one, and its folder stays where it is.

## What waits on you

`agent todo` prints one line per item that waits on the operator, oldest first: its age, the
project, the question on one line and, for a request, the chain that holds it.

```
7d   garden: Approve the release build (overdue, admin waits on garden, request 8102f4)
2h   admin: Which key goes in 1Password (garden waits on alice, request 8102f4)
```

Its sources are the Pending approvals bullets of every active project's `notes/STATE.md`, aged
by that file's modification time, open requests older than a day, and replies that answered
`--needs-jan`. A request is open until a reply names it in `inReplyTo`, so an answered item stays
out. `agent todo --all` prints the flat list of every question and waiting job instead.

`agent reply <project> <id> <text> --needs-jan` answers a request that cannot finish without the
operator, and keeps it in the list. Any other reply is `outcome: done` and closes the request.

## Doctor

```sh
agent doctor              # lint every active project on this host
agent doctor home         # lint one project
```

One read-only pass over what a script can decide and what breaks recovery: the level-two headings
of `notes/PROJECT.md`, `notes/STATE.md` and every topic page, the knowledge index links, the
mandatory context budget in `~/.config/agents/standards/manifest.json` along with its file hashes,
the `.agent` state and local-only member directories that Git cannot restore, em and en dashes or
ellipses in `notes/*.md`, and the watchdog and jobs timers. It prints one block per project that
has problems, prints `ok` when nothing is wrong, and exits 1 when it found one. It never writes and
never fixes anything.

## Schedules

```sh
agent schedule add audit garden "daily 09:00" "Check the alerts"
agent schedule add sweep admin "weekly mon 08:00" "Review last week's commits"
agent schedule add poll garden "every 6h" "Check the queue"
agent schedule ls
agent schedule rm audit
```

A cadence is `daily HH:MM`, `weekly mon HH:MM` or `every Nh`, in Europe/Warsaw time. One JSON
file per schedule lives in `~/.local/state/agents/schedules`, and a schedule runs on the host of
its project. `agent watch`, which the host's timer already runs every ten minutes, delivers each
due slot into the project's inbox as a request from `schedule`, once per slot, and records the
slot in the schedule file. A slot missed while the machine was off runs once. A new schedule
waits for its next slot. `agent watch --dry-run` prints what would run and writes nothing.

The receiving agent answers with `agent reply` like any other request. An answer that needs the
operator carries `--needs-jan` and appears in `agent todo`.

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
    "hub": {"root": "~/Projects", "meminiPrefix": "lab", "guard": "personal", "scratchProject": "workshop"},
    "outpost": {"root": "~/Field", "meminiPrefix": "field", "guard": "work", "maxAgents": 5}
  },
  "defaults": {"host": "hub", "model": "provider/model", "thinking": "high", "tasks": {"maxOpen": 4}},
  "projects": {
    "admin": {},
    "garden": {"repos": [{"name": "code", "url": "https://example.com/code.git"}]},
    "workshop": {},
    "field": {"host": "outpost"}
  }
}
```

## Tests

The suite needs Go and Git. Every other tool it meets (Rex, Pi, mise, ssh, chezmoi) is a stub.

```sh
go test ./tests                                              # builds ./cmd/agent
AGENT_BIN=/path/to/agent go test -count=1 ./tests            # a prebuilt command
```

## License

MIT
