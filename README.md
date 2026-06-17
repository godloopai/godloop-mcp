# godloop CLI + MCP

Public command-line tools for [godloop.ai](https://godloop.ai) — an AI
tokenmaxxing productivity app that makes you use the tokens you already pay
for.

This repo ships two binaries:

- `godloop`: the local runner CLI. Use this first.
- `godloop-mcp`: the stdio MCP connector for native MCP sessions.

## Install the runner

With Go:

```bash
go install github.com/godloopai/godloop-mcp/cmd/godloop@latest
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

`godloop-mcp` exposes one MCP tool, `loop`. Call it at the top of every `/loop`
tick: it reports the previous tick, returns the next task to work on, shows
usage across your AI subs, and tells your agent when to schedule the next tick.

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
