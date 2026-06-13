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

This drops `godloop-mcp` into `$(go env GOPATH)/bin` — make sure that's on
your `PATH` (`export PATH="$PATH:$(go env GOPATH)/bin"`), or the
`claude mcp add` step below won't find the binary.

No Go? Grab a prebuilt binary from
[Releases](https://github.com/godloopai/godloop-mcp/releases) (linux/macOS
amd64+arm64, windows amd64):

```bash
curl -fL -o godloop-mcp \
  https://github.com/godloopai/godloop-mcp/releases/latest/download/godloop-mcp_linux_amd64
chmod +x godloop-mcp
sudo mv godloop-mcp /usr/local/bin/
```

(Swap `linux_amd64` for your platform; on macOS you may need
`xattr -d com.apple.quarantine /usr/local/bin/godloop-mcp` the first run.)

## Register with Claude Code

```bash
claude mcp add godloop --env GODLOOP_KEY=<your-key> -- godloop-mcp
```

Get your key at [godloop.ai](https://godloop.ai) — dashboard → ai subs →
api keys → create.

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

The `.godloop` file holds either the raw project id or
`{"project_id": "..."}`.

## Build from source

```bash
CGO_ENABLED=0 go build -o godloop-mcp .
```

No dependencies — Go stdlib only.
