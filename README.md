# godloop CLI + MCP

Public command-line tools for [godloop.ai](https://godloop.ai) — an AI
tokenmaxxing productivity app that makes you use the tokens you already pay
for.

This repo ships two binaries:

- `godloop`: the local runner CLI. Use this first.
- `godloop-mcp`: the stdio MCP connector for native MCP sessions.

The MCP connector also exposes a read-only project dashboard and an
account-native `masterplan` tool. It reads the authenticated user's living plan
and safely creates, updates, or explicitly confirmed-deletes project-linked
nodes through Godloop.

## Install the runner

With Go:

```bash
go install github.com/godloopai/godloop-mcp/cmd/godloop@v0.6.0-alpha
godloop
```

This does not require `sudo`. It drops `godloop` into `$(go env GOPATH)/bin` —
make sure that directory is on your `PATH`:

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
```

No Go? Grab a prebuilt binary from
[Releases](https://github.com/godloopai/godloop-mcp/releases) (linux/macOS
amd64+arm64, windows amd64):

```bash
curl -fL -o godloop \
  https://github.com/godloopai/godloop-mcp/releases/latest/download/godloop_linux_amd64
chmod +x godloop
mkdir -p "$HOME/.local/bin"
mv godloop "$HOME/.local/bin/"
```

No `sudo` is needed if `$HOME/.local/bin` is on your `PATH`. You only need
`sudo` if you choose to move the binary into a system directory like
`/usr/local/bin`.

Swap `linux_amd64` for your platform. On macOS you may need:

```bash
xattr -d com.apple.quarantine "$HOME/.local/bin/godloop"
```

## Use the runner

```bash
godloop
godloop run
```

`godloop` opens browser login if needed, shows your available workspaces, and
lets you choose or create one. `godloop run` keeps this machine available for
work queued from the dashboard and checks for new work about every 10 seconds.
It uses outbound HTTPS with your runner key; the service does not SSH into your
machine.

For a workspace with several repositories, point the runner at their common
parent:

```bash
godloop run -workdir /path/to/projects/godloop -projects-root /path/to/projects
```

Before claiming work, the runner verifies that a checkout's `.godloop` matches
the server project. If a saved location is stale it finds a unique matching
direct child under `-projects-root`; missing or ambiguous mappings are skipped
instead of running work in the wrong repository.

By default, Codex runs with `--sandbox danger-full-access` so local shell tools
work on hosts where Codex's bubblewrap sandbox cannot create network namespaces.
Run godloop only in repos and machines you trust, or put the whole runner inside
Docker/devcontainer.

For provider bypass modes, use `-danger` only inside Docker/devcontainer or
another isolated environment:

```bash
godloop run -agent codex -workdir /path/to/repo -danger
```

If you want Codex's stricter sandbox and your host supports it, opt back in:

```bash
godloop run -agent codex -workdir /path/to/repo -codex-sandbox workspace-write
```

While a prompt is running, the CLI tees provider output to your terminal and
sends bounded progress summaries back to godloop every 20 seconds. Use
`-progress-interval 0` to disable live progress reports for a run.

Advanced/manual commands are still available:

```bash
godloop login
godloop status
godloop once -project <project-id> -agent codex -workdir /path/to/repo
godloop loop -project <project-id> -workdir /path/to/repo
```

## Install the MCP connector

With Go:

```bash
go install github.com/godloopai/godloop-mcp@latest
```

Or download `godloop-mcp_<platform>_<arch>` from
[Releases](https://github.com/godloopai/godloop-mcp/releases).

`godloop-mcp` exposes five MCP tools: `loop` (the tick), `projects` (read-only
status), `loops` (CRUD your loop templates), `godloop` (compose and drive
godloops), and `masterplan` — see Tools below.

## Register with Codex

Register the connector once in your user config:

```bash
codex mcp add godloop -- godloop-mcp
```

When authentication comes from `godloop login`, use a small wrapper that reads
`~/.config/godloop/config.json` at startup and exports `GODLOOP_KEY` and
`GODLOOP_URL` before launching `godloop-mcp`. Codex stores user-level MCP
servers in `~/.codex/config.toml`; every trusted repository then selects its
project through its local `.godloop` file. Restart Codex (not your shell) after
adding or updating the connector.

## Register with Claude Code

```bash
claude mcp add godloop --env GODLOOP_KEY=<your-key> -- godloop-mcp
```

Use `godloop login` for the normal machine connection flow. Manual
`GODLOOP_KEY` handling is an advanced fallback for MCP-only setups.

Then in each repo you want godloop to pull tasks for, drop a `.godloop`
file with the project id (shown on your project's page at godloop.ai):

```bash
echo '<project-id>' > .godloop
```

## Tools

- `loop` — the tick: call at the top of every /loop iteration. Reports the
  previous tick, returns the next task, usage across your AI subs, and when
  to call again.
- `projects` — read-only live status. `current` uses the repository's
  `.godloop`; `overview` returns the cross-project dashboard with runner,
  task, Loop, Godloop, inbox, and latest-run status.
- `loops` — CRUD your loop templates (`list | get | create | update | delete`).
  `get` an existing loop to learn the `config_json` steps shape. Visibility can
  be set to `private`/`unlisted`; publishing to the marketplace needs the
  dashboard.
- `godloop` — compose and inspect godloops. Template actions
  (`list_templates | get_template | create_template | update_template | delete_template`)
  edit the recipe: send the full ordered `loop_template_ids` list to add,
  remove, or reorder members. Instance actions (`list | get | reorder | trigger`)
  drive a godloop assigned to a project: `get` returns members in order plus an
  `active` block with the running member index and cycle number; `reorder`
  mid-cycle lets the running loop finish and applies the new order after it.
- `masterplan` — `read` returns the authenticated account masterplan.
  `create | update | delete` change one node using the exact returned revision;
  delete additionally requires `confirm_node_id` to repeat the target id.

`loops` and `godloop` need the same paid `GODLOOP_KEY` as `loop`.

## Config

| Variable | Required | Default |
|---|---|---|
| `GODLOOP_KEY` | yes | — |
| `GODLOOP_URL` | no | `https://godloop.ai` |
| `GODLOOP_PROJECT` | no | read from `.godloop` file in project root |
| `GODLOOP_MAX_PROMPT_CHARS` | no | `4000` |
| `GODLOOP_AUTO_UPDATE` | no | `notify` |

The `.godloop` file holds either the raw project id or
`{"project_id": "..."}`.

`GODLOOP_MAX_PROMPT_CHARS` bounds how much of the claimed prompt is returned to
the MCP client. The server also caps this, but keeping the client default low
prevents stale context from eating tokens.

`GODLOOP_AUTO_UPDATE` controls startup update checks:

- `off`: no update check
- `notify`: print an update notice to stderr only
- `minor`: run `go install github.com/godloopai/godloop-mcp@latest` for newer versions with the same major version
- `always`: run that update command for any newer version

Auto-update replaces the installed binary for the next MCP process; restart the
MCP client to run the new version.

## Build from source

```bash
CGO_ENABLED=0 go build -o godloop-mcp .
CGO_ENABLED=0 go build -o godloop ./cmd/godloop
```

No dependencies — Go stdlib only.
