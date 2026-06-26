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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const version = "0.3.2-alpha"

const (
	defaultMaxPromptChars = 4000
	maxPromptChars        = 20000
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type updateManifest struct {
	LatestVersion       string   `json:"latest_version"`
	MinSupportedVersion string   `json:"min_supported_version"`
	DefaultPolicy       string   `json:"default_policy"`
	Policies            []string `json:"policies"`
	InstallCommand      string   `json:"install_command"`
	ReleasesURL         string   `json:"releases_url"`
}

type apiEnvelope[T any] struct {
	Data T `json:"data"`
}

type loopResp struct {
	EnvironmentID     int64      `json:"environment_id"`
	Action            string     `json:"action"` // work | idle | backoff
	Reason            string     `json:"reason"`
	Task              *loopTask  `json:"task"`
	Subs              []subUsage `json:"subs"`
	NextCallSeconds   int64      `json:"next_call_seconds"`
	ServerTime        int64      `json:"server_time"`
	MaxPromptChars    int        `json:"max_prompt_chars"`
	ContextBudgetHint string     `json:"context_budget_hint"`
}

type loopTask struct {
	ID              int64  `json:"id"`
	ProjectID       string `json:"project_id"`
	Title           string `json:"title"`
	Prompt          string `json:"prompt"`
	Command         string `json:"command"`
	Status          string `json:"status"`
	ClaimedBy       *int64 `json:"claimed_by"`
	ClaimExpiresAt  int64  `json:"claim_expires_at"`
	PromptTruncated bool   `json:"prompt_truncated"`
}

type subUsage struct {
	ID                    int64  `json:"id"`
	Name                  string `json:"name"`
	Type                  string `json:"type"`
	Status                string `json:"status"`
	EstTokensUsed         int64  `json:"est_tokens_used"`
	WeeklyTokenAllowance  int64  `json:"weekly_token_allowance"`
	ResetAt               int64  `json:"reset_at"`
	SessionTokens         int64  `json:"session_tokens"`
	SessionStartedAt      int64  `json:"session_started_at"`
	SessionTokenAllowance int64  `json:"session_token_allowance"`
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

func baseURL() string {
	base := strings.TrimRight(os.Getenv("GODLOOP_URL"), "/")
	if base == "" {
		base = "https://godloop.ai"
	}
	return base
}

func clampInt(v, fallback, max int) int {
	if v <= 0 {
		return fallback
	}
	if v > max {
		return max
	}
	return v
}

func configuredMaxPromptChars() int {
	if raw := strings.TrimSpace(os.Getenv("GODLOOP_MAX_PROMPT_CHARS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			return clampInt(n, defaultMaxPromptChars, maxPromptChars)
		}
	}
	return defaultMaxPromptChars
}

func parseSemver(v string) (int, int, int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, false
	}
	patch := 0
	if len(parts) > 2 {
		patchPart := parts[2]
		if i := strings.IndexFunc(patchPart, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			patchPart = patchPart[:i]
		}
		patch, _ = strconv.Atoi(patchPart)
	}
	return major, minor, patch, true
}

func versionGreater(a, b string) bool {
	amaj, amin, apat, okA := parseSemver(a)
	bmaj, bmin, bpat, okB := parseSemver(b)
	if !okA || !okB {
		return a != "" && a != b
	}
	if amaj != bmaj {
		return amaj > bmaj
	}
	if amin != bmin {
		return amin > bmin
	}
	return apat > bpat
}

func sameMajor(a, b string) bool {
	amaj, _, _, okA := parseSemver(a)
	bmaj, _, _, okB := parseSemver(b)
	return okA && okB && amaj == bmaj
}

func fetchUpdateManifest() (updateManifest, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL() + "/api/v1/mcp/version")
	if err != nil {
		return updateManifest{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return updateManifest{}, fmt.Errorf("version endpoint returned %d", resp.StatusCode)
	}
	var envelope apiEnvelope[updateManifest]
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		return updateManifest{}, err
	}
	return envelope.Data, nil
}

func updatePolicy() string {
	policy := strings.ToLower(strings.TrimSpace(os.Getenv("GODLOOP_AUTO_UPDATE")))
	if policy == "" {
		policy = "notify"
	}
	switch policy {
	case "off", "notify", "minor", "always":
		return policy
	default:
		return "notify"
	}
}

func shouldInstallUpdate(policy string, manifest updateManifest) bool {
	if !versionGreater(manifest.LatestVersion, version) {
		return false
	}
	switch policy {
	case "always":
		return true
	case "minor":
		return sameMajor(manifest.LatestVersion, version)
	default:
		return false
	}
}

func checkForUpdate() {
	policy := updatePolicy()
	if policy == "off" {
		return
	}
	manifest, err := fetchUpdateManifest()
	if err != nil || !versionGreater(manifest.LatestVersion, version) {
		return
	}
	msg := fmt.Sprintf("godloop-mcp %s available; current %s", manifest.LatestVersion, version)
	if manifest.InstallCommand != "" {
		msg += "; update with: " + manifest.InstallCommand
	}
	if !shouldInstallUpdate(policy, manifest) {
		fmt.Fprintln(os.Stderr, msg)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "install", "github.com/godloopai/godloop-mcp@latest")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s; auto-update failed: %v\n", msg, err)
		return
	}
	fmt.Fprintf(os.Stderr, "godloop-mcp updated to %s; restart the MCP client to use it\n", manifest.LatestVersion)
}

var loopTool = map[string]any{
	"name": "loop",
	"description": "godloop tick: call once at the start of every /loop iteration. " +
		"Pass a report of what happened last tick (task outcome + token usage). " +
		"Returns an action you must honour — work (do the returned task), idle " +
		"(no work; just wait), or backoff (a usage limit is near; do NOT run work, " +
		"wait the full interval) — plus the next task, token usage across your AI " +
		"subscriptions with reset times, and next_call_seconds: schedule the next " +
		"/loop tick that many seconds from now.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"report": map[string]any{
				"type":        "object",
				"description": "what happened since the previous loop call",
				"properties": map[string]any{
					"task_id":          map[string]any{"type": "integer"},
					"outcome":          map[string]any{"type": "string", "enum": []string{"done", "progress", "error"}},
					"summary":          map[string]any{"type": "string", "description": "one-line result"},
					"input_tokens":     map[string]any{"type": "integer", "description": "estimated input tokens spent"},
					"output_tokens":    map[string]any{"type": "integer", "description": "estimated output tokens spent"},
					"session_used":     map[string]any{"type": "integer", "description": "output tokens used in the current 5h provider window"},
					"session_limit":    map[string]any{"type": "integer", "description": "the 5h output-token cap (from GODLOOP_SESSION_TOKEN_LIMIT)"},
					"session_reset_at": map[string]any{"type": "integer", "description": "unix time the 5h window resets"},
				},
			},
			"ai_sub_id": map[string]any{
				"type":        "integer",
				"description": "which AI subscription this environment burns (optional)",
			},
			"max_prompt_chars": map[string]any{
				"type":        "integer",
				"description": "max prompt characters to return for the claimed task; defaults to a compact 4000",
			},
		},
	},
}

func callLoop(args json.RawMessage) (string, error) {
	key := os.Getenv("GODLOOP_KEY")
	if key == "" {
		return "", fmt.Errorf("GODLOOP_KEY not set")
	}
	base := baseURL()
	pid := projectID()
	if pid == "" {
		return "", fmt.Errorf("no project id: create a .godloop file with your project's public id (see godloop.ai dashboard)")
	}

	var in struct {
		Report         map[string]any `json:"report"`
		AISubID        *int64         `json:"ai_sub_id"`
		MaxPromptChars int            `json:"max_prompt_chars"`
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
	// real 5h-block session usage, for backend backoff decisions
	if su, rst := measureSession(); su > 0 {
		if in.Report == nil {
			in.Report = map[string]any{}
		}
		in.Report["session_used"] = su
		in.Report["session_reset_at"] = rst
		if lim := sessionTokenLimit(); lim > 0 {
			in.Report["session_limit"] = lim
		}
	}
	host, _ := os.Hostname()
	body := map[string]any{
		"project_id":       pid,
		"name":             host,
		"kind":             kind(),
		"max_prompt_chars": clampInt(in.MaxPromptChars, configuredMaxPromptChars(), maxPromptChars),
	}
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
	return compactLoopOutput(out), nil
}

func compactLoopOutput(raw []byte) string {
	var envelope apiEnvelope[loopResp]
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return string(raw)
	}
	resp := envelope.Data
	var b strings.Builder
	b.WriteString("godloop loop\n")
	fmt.Fprintf(&b, "action: %s — %s\n", resp.Action, resp.Reason)
	if resp.Task == nil {
		b.WriteString("task: none\n")
	} else {
		fmt.Fprintf(&b, "task: #%d %s\n", resp.Task.ID, resp.Task.Title)
		if resp.Task.Prompt != "" {
			b.WriteString("prompt:\n")
			b.WriteString(resp.Task.Prompt)
			b.WriteString("\n")
		}
		if resp.Task.PromptTruncated {
			b.WriteString("prompt_truncated: true\n")
		}
	}
	if len(resp.Subs) > 0 {
		b.WriteString("subscriptions:\n")
		for _, sub := range resp.Subs {
			fmt.Fprintf(&b, "- %s (%s): status=%s", sub.Name, sub.Type, sub.Status)
			if sub.SessionTokenAllowance > 0 {
				fmt.Fprintf(&b, ", session=%d/%d", sub.SessionTokens, sub.SessionTokenAllowance)
			}
			if sub.WeeklyTokenAllowance > 0 {
				fmt.Fprintf(&b, ", weekly=%d/%d", sub.EstTokensUsed, sub.WeeklyTokenAllowance)
			}
			b.WriteString("\n")
		}
	}
	if resp.MaxPromptChars > 0 {
		fmt.Fprintf(&b, "max_prompt_chars: %d\n", resp.MaxPromptChars)
	}
	if resp.ContextBudgetHint != "" {
		fmt.Fprintf(&b, "context_budget_hint: %s\n", resp.ContextBudgetHint)
	}
	fmt.Fprintf(&b, "next_call_seconds: %d", resp.NextCallSeconds)
	return b.String()
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

// --- 5h-block session usage ---
// Providers (e.g. Claude) cap output tokens per fixed 5h window. The backend's
// loopDecision backs off when a runner reports session_used/limit/reset_at, so
// we compute output tokens spent in the current window from the same transcripts.

// computeBlockStart returns the start of the fixed 5h block containing t: a :59
// boundary on a UTC hour divisible by 5 (00:59, 05:59, 10:59, 15:59, 20:59 UTC).
// Pure; mirrors the Python governor.
func computeBlockStart(t time.Time) time.Time {
	u := t.UTC()
	b := time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 59, 0, 0, time.UTC)
	if b.After(t) {
		b = b.Add(-time.Hour)
	}
	// floor the hour to the nearest 5h boundary (0,5,10,15,20)
	return time.Date(b.Year(), b.Month(), b.Day(), (b.Hour()/5)*5, 59, 0, 0, time.UTC)
}

// scanSessionOutput sums output_tokens for transcript entries at/after since.
func scanSessionOutput(path string, since time.Time) int64 {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	var out int64
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
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Timestamp.Before(since) {
			continue
		}
		out += entry.Message.Usage.OutputTokens
	}
	return out
}

// measureSession returns (output tokens used in the current 5h block, unix time
// the block resets). Files untouched since the block start can't hold in-block
// entries, so they're skipped. Any FS error degrades to (0, 0) — no panic.
func measureSession() (used int64, resetAt int64) {
	start := computeBlockStart(time.Now().UTC())
	reset := start.Add(5 * time.Hour)

	home, err := os.UserHomeDir()
	if err != nil {
		return 0, 0
	}
	dirs, err := os.ReadDir(home + "/.claude/projects")
	if err != nil {
		return 0, 0
	}
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
			if info, err := f.Info(); err != nil || info.ModTime().Before(start) {
				continue // untouched since block start
			}
			used += scanSessionOutput(dir+"/"+f.Name(), start)
		}
	}
	return used, reset.Unix()
}

// sessionTokenLimit reads the optional 5h output-token cap from the environment;
// 0 when unset, empty, unparseable, or negative.
func sessionTokenLimit() int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("GODLOOP_SESSION_TOKEN_LIMIT")), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func main() {
	go checkForUpdate()
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
				"serverInfo":      map[string]any{"name": "godloop", "version": version},
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
