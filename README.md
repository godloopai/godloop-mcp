# godloop-mcp

Tiny stdio MCP client for [godloop.ai](https://godloop.ai) — an AI
tokenmaxxing productivity app that makes you use the tokens you already pay
for.

It exposes one tool, `loop`. Call it at the top of every `/loop` tick: it
reports the previous tick, returns the next task to work on, shows usage
across your AI subs, and tells your agent when to schedule the next tick.

## Install

With Go:

```bash
go install github.com/godloopai/godloop-mcp@latest
```

Or grab a prebuilt binary from
[Releases](https://github.com/godloopai/godloop-mcp/releases) (linux/macOS
amd64+arm64, windows amd64).

## Register with Claude Code

```bash
claude mcp add godloop --env GODLOOP_KEY=<your-key> -- godloop-mcp
```

Get your key at [godloop.ai](https://godloop.ai).

## Config

| Variable | Required | Default |
|---|---|---|
| `GODLOOP_KEY` | yes | — |
| `GODLOOP_URL` | no | `https://godloop.ai` |
| `GODLOOP_PROJECT` | no | read from `.godloop` file in project root |

The `.godloop` file holds either the raw project id or
`{"project_id": "..."}`.

## Build from source

```bash
CGO_ENABLED=0 go build -o godloop-mcp .
```

No dependencies — Go stdlib only.
