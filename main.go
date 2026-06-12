// godloop-mcp: tiny stdio MCP server exposing one tool, "loop".
// Call it at the top of every /loop tick: it reports the previous tick,
// returns the next task to work on, usage across your AI subs, and when
// to schedule the next tick.
//
// Config: GODLOOP_KEY (required), GODLOOP_URL (default https://godloop.ai),
// project id from ./.godloop (raw public id, or {"project_id":"..."}).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func reply(id json.RawMessage, result any) {
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	fmt.Println(string(out))
}

func replyErr(id json.RawMessage, code int, msg string) {
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
	fmt.Println(string(out))
}

func projectID() string {
	if v := os.Getenv("GODLOOP_PROJECT"); v != "" {
		return v
	}
	raw, err := os.ReadFile(".godloop")
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	var f struct {
		ProjectID string `json:"project_id"`
	}
	if json.Unmarshal(raw, &f) == nil && f.ProjectID != "" {
		return f.ProjectID
	}
	return s
}

var loopTool = map[string]any{
	"name": "loop",
	"description": "godloop tick: call once at the start of every /loop iteration. " +
		"Pass a report of what happened last tick (task outcome + token usage). " +
		"Returns the next task to work on (claim it by working on it), token usage " +
		"across your AI subscriptions with reset times, and next_call_seconds — " +
		"schedule the next /loop tick that many seconds from now.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"report": map[string]any{
				"type":        "object",
				"description": "what happened since the previous loop call",
				"properties": map[string]any{
					"task_id":       map[string]any{"type": "integer"},
					"outcome":       map[string]any{"type": "string", "enum": []string{"done", "progress", "error"}},
					"summary":       map[string]any{"type": "string", "description": "one-line result"},
					"input_tokens":  map[string]any{"type": "integer", "description": "estimated input tokens spent"},
					"output_tokens": map[string]any{"type": "integer", "description": "estimated output tokens spent"},
				},
			},
			"ai_sub_id": map[string]any{
				"type":        "integer",
				"description": "which AI subscription this environment burns (optional)",
			},
		},
	},
}

func callLoop(args json.RawMessage) (string, error) {
	key := os.Getenv("GODLOOP_KEY")
	if key == "" {
		return "", fmt.Errorf("GODLOOP_KEY not set")
	}
	base := os.Getenv("GODLOOP_URL")
	if base == "" {
		base = "https://godloop.ai"
	}
	pid := projectID()
	if pid == "" {
		return "", fmt.Errorf("no project id: create a .godloop file with your project's public id (see godloop.ai dashboard)")
	}

	var in struct {
		Report  map[string]any `json:"report"`
		AISubID *int64         `json:"ai_sub_id"`
	}
	if len(args) > 0 {
		json.Unmarshal(args, &in)
	}
	// measured transcript tokens beat model guesses
	if mi, mo := measureUsage(); mi+mo > 0 {
		if in.Report == nil {
			in.Report = map[string]any{}
		}
		in.Report["input_tokens"] = mi
		in.Report["output_tokens"] = mo
	}
	host, _ := os.Hostname()
	body := map[string]any{"project_id": pid, "name": host, "kind": kind()}
	if in.Report != nil {
		body["report"] = in.Report
	}
	if in.AISubID != nil {
		body["ai_sub_id"] = *in.AISubID
	}
	payload, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", base+"/api/v1/mcp/loop", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Godloop-Key", key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("godloop api %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func kind() string {
	if os.Getenv("GODLOOP_KIND") == "vps" {
		return "vps"
	}
	return "local"
}

// --- token measurement: parse Claude Code transcript JSONL deltas ---
// Claude Code logs exact per-message token usage to
// ~/.claude/projects/<project>/<session>.jsonl. We sum everything newer than
// the last scan, so reported numbers are measured, not model guesses.
// cache_read tokens are ignored (cheap); cache_creation counts as input.

type scanState struct {
	LastScanUnix int64 `json:"last_scan_unix"`
}

func statePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.godloop-mcp-state.json"
}

func loadState() scanState {
	var st scanState
	if raw, err := os.ReadFile(statePath()); err == nil {
		json.Unmarshal(raw, &st)
	}
	return st
}

func saveState(st scanState) {
	if p := statePath(); p != "" {
		raw, _ := json.Marshal(st)
		os.WriteFile(p, raw, 0o600)
	}
}

// measureUsage returns (input, output) tokens logged since the last scan.
// First ever run anchors the state and reports zero — avoids dumping all
// history into the weekly window.
func measureUsage() (int64, int64) {
	st := loadState()
	now := time.Now().Unix()
	defer saveState(scanState{LastScanUnix: now})
	if st.LastScanUnix == 0 {
		return 0, 0
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return 0, 0
	}
	dirs, err := os.ReadDir(home + "/.claude/projects")
	if err != nil {
		return 0, 0
	}
	since := time.Unix(st.LastScanUnix, 0)
	var in, out int64
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := home + "/.claude/projects/" + d.Name()
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			if info, err := f.Info(); err != nil || info.ModTime().Before(since) {
				continue // untouched since last scan
			}
			i, o := scanJSONL(dir+"/"+f.Name(), since)
			in += i
			out += o
		}
	}
	return in, out
}

func scanJSONL(path string, since time.Time) (int64, int64) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	var in, out int64
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		var entry struct {
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Usage struct {
					InputTokens         int64 `json:"input_tokens"`
					CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
					OutputTokens        int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Timestamp.Before(since) {
			continue
		}
		in += entry.Message.Usage.InputTokens + entry.Message.Usage.CacheCreationTokens
		out += entry.Message.Usage.OutputTokens
	}
	return in, out
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "godloop", "version": "0.1.0"},
			})
		case "notifications/initialized":
			// notification, no response
		case "tools/list":
			reply(req.ID, map[string]any{"tools": []any{loopTool}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			if p.Name != "loop" {
				replyErr(req.ID, -32602, "unknown tool: "+p.Name)
				continue
			}
			text, err := callLoop(p.Arguments)
			isErr := err != nil
			if isErr {
				text = err.Error()
			}
			reply(req.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}},
				"isError": isErr,
			})
		case "ping":
			reply(req.ID, map[string]any{})
		default:
			if req.ID != nil {
				replyErr(req.ID, -32601, "method not found: "+req.Method)
			}
		}
	}
}
