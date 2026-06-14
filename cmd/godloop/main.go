package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultAPIURL = "https://godloop.ai"

type config struct {
	APIURL  string `json:"api_url"`
	Key     string `json:"key"`
	Machine string `json:"machine"`
}

type apiEnvelope[T any] struct {
	Data    T      `json:"data"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

type runnerSession struct {
	Code                string `json:"code"`
	Name                string `json:"name"`
	Status              string `json:"status"`
	VerifyURL           string `json:"verify_url"`
	Key                 string `json:"key"`
	ExpiresAt           int64  `json:"expires_at"`
	PollIntervalSeconds int64  `json:"poll_interval_seconds"`
}

type runnerStatus struct {
	ServerTime  int64         `json:"server_time"`
	Projects    []project     `json:"projects"`
	Subs        []subUsage    `json:"subs"`
	CurrentWork []currentWork `json:"current_work"`
}

type project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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

type currentWork struct {
	ProjectID       string `json:"project_id"`
	ProjectName     string `json:"project_name"`
	TaskID          int64  `json:"task_id"`
	TaskTitle       string `json:"task_title"`
	PromptPreview   string `json:"prompt_preview"`
	EnvironmentID   int64  `json:"environment_id"`
	EnvironmentName string `json:"environment_name"`
	ClaimExpiresAt  int64  `json:"claim_expires_at"`
}

type loopResponse struct {
	EnvironmentID   int64     `json:"environment_id"`
	Task            *loopTask `json:"task"`
	NextCallSeconds int64     `json:"next_call_seconds"`
	ServerTime      int64     `json:"server_time"`
	MaxPromptChars  int       `json:"max_prompt_chars"`
	ContextHint     string    `json:"context_budget_hint"`
}

type loopTask struct {
	ID              int64  `json:"id"`
	ProjectID       string `json:"project_id"`
	Title           string `json:"title"`
	Prompt          string `json:"prompt"`
	Command         string `json:"command"`
	Status          string `json:"status"`
	PromptTruncated bool   `json:"prompt_truncated"`
}

type usage struct {
	Input  int64
	Output int64
}

func main() {
	if len(os.Args) < 2 {
		usageAndExit()
	}
	var err error
	switch os.Args[1] {
	case "login", "connect":
		err = login(os.Args[2:])
	case "status", "usage":
		err = status(os.Args[2:])
	case "once":
		err = once(os.Args[2:])
	case "logout":
		err = logout()
	default:
		usageAndExit()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usageAndExit() {
	fmt.Fprintln(os.Stderr, `usage:
  godloop login [-api https://godloop.ai] [-name machine]
  godloop status [-api https://godloop.ai] [-key glp_...]
  godloop usage
  godloop once -project <id> [-env name] [-agent codex|claude] [-workdir .] [-danger]
  godloop logout`)
	os.Exit(2)
}

func login(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	apiURL := fs.String("api", envOrDefault("GODLOOP_API_URL", defaultAPIURL), "godloop API URL")
	name := fs.String("name", hostname(), "machine name")
	timeout := fs.Duration("timeout", 10*time.Minute, "pairing timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var created runnerSession
	if err := apiRequest(context.Background(), "POST", *apiURL, "/api/v1/runner/sessions", "", map[string]string{"name": *name}, &created); err != nil {
		return err
	}
	fmt.Println("Open this link to connect the machine:")
	fmt.Println(created.VerifyURL)
	fmt.Println()
	fmt.Println("Waiting for browser approval...")

	interval := time.Duration(created.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		var polled runnerSession
		if err := apiRequest(context.Background(), "GET", *apiURL, "/api/v1/runner/sessions/"+created.Code, "", nil, &polled); err != nil {
			return err
		}
		switch polled.Status {
		case "approved":
			if polled.Key == "" {
				continue
			}
			cfg := config{APIURL: strings.TrimRight(*apiURL, "/"), Key: polled.Key, Machine: *name}
			if err := saveConfig(cfg); err != nil {
				return err
			}
			fmt.Println("Connected", *name)
			return nil
		case "expired":
			return errors.New("pairing code expired")
		case "consumed":
			return errors.New("pairing code was already consumed")
		}
	}
	return errors.New("timed out waiting for approval")
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	apiURL := fs.String("api", "", "godloop API URL")
	key := fs.String("key", "", "godloop runner key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _ := loadConfig()
	if *apiURL == "" {
		*apiURL = firstNonEmpty(cfg.APIURL, envOrDefault("GODLOOP_API_URL", defaultAPIURL))
	}
	if *key == "" {
		*key = firstNonEmpty(os.Getenv("GODLOOP_KEY"), cfg.Key)
	}
	if *key == "" {
		return errors.New("not connected; run godloop login first")
	}

	var st runnerStatus
	if err := apiRequest(context.Background(), "GET", *apiURL, "/api/v1/runner/status", *key, nil, &st); err != nil {
		return err
	}
	printStatus(st)
	return nil
}

func once(args []string) error {
	fs := flag.NewFlagSet("once", flag.ExitOnError)
	apiURL := fs.String("api", "", "godloop API URL")
	key := fs.String("key", "", "godloop runner key")
	projectID := fs.String("project", "", "project id from godloop")
	envName := fs.String("env", hostname(), "environment name")
	kind := fs.String("kind", "local", "environment kind")
	agent := fs.String("agent", "codex", "codex or claude")
	workdir := fs.String("workdir", ".", "repo directory")
	danger := fs.Bool("danger", false, "use provider bypass/danger mode; run inside a container")
	subID := fs.Int64("sub", 0, "AI sub id to charge usage against")
	maxPromptChars := fs.Int("max-prompt-chars", 8000, "max prompt chars to request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*projectID) == "" {
		return errors.New("-project is required")
	}
	cfg, _ := loadConfig()
	if *apiURL == "" {
		*apiURL = firstNonEmpty(cfg.APIURL, envOrDefault("GODLOOP_API_URL", defaultAPIURL))
	}
	if *key == "" {
		*key = firstNonEmpty(os.Getenv("GODLOOP_KEY"), cfg.Key)
	}
	if *key == "" {
		return errors.New("not connected; run godloop login first")
	}

	body := map[string]any{
		"project_id":       strings.TrimSpace(*projectID),
		"name":             clean(*envName),
		"kind":             clean(*kind),
		"max_prompt_chars": *maxPromptChars,
	}
	if *subID > 0 {
		body["ai_sub_id"] = *subID
	}
	var loop loopResponse
	if err := apiRequest(context.Background(), "POST", *apiURL, "/api/v1/mcp/loop", *key, body, &loop); err != nil {
		return err
	}
	if loop.Task == nil {
		fmt.Println("No queued prompt.")
		fmt.Printf("Next check: %s\n", time.Duration(loop.NextCallSeconds)*time.Second)
		return nil
	}
	fmt.Printf("Running #%d: %s\n", loop.Task.ID, loop.Task.Title)
	if loop.Task.PromptTruncated {
		fmt.Println("Prompt was truncated by server max_prompt_chars.")
	}

	out, use, runErr := runAgent(*agent, *workdir, loop.Task.Prompt, *danger)
	outcome := "done"
	if runErr != nil {
		outcome = "error"
	}
	summary := summarizeOutput(out)
	report := map[string]any{
		"environment_id": loop.EnvironmentID,
		"task_id":        loop.Task.ID,
		"outcome":        outcome,
		"summary":        summary,
		"input_tokens":   use.Input,
		"output_tokens":  use.Output,
	}
	if *subID > 0 {
		report["ai_sub_id"] = *subID
	}
	var reportResp map[string]bool
	if err := apiRequest(context.Background(), "POST", *apiURL, "/api/v1/mcp/report", *key, report, &reportResp); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	fmt.Println(summary)
	return nil
}

func logout() error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Println("Logged out.")
	return nil
}

func runAgent(agent, workdir, prompt string, danger bool) (string, usage, error) {
	switch strings.ToLower(agent) {
	case "codex":
		args := []string{"exec", "--cd", workdir, "--json"}
		if danger {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		} else {
			args = append(args, "--sandbox", "workspace-write")
		}
		args = append(args, prompt)
		return runCommand("codex", args...)
	case "claude":
		args := []string{"-p", "--output-format", "json"}
		if danger {
			args = append(args, "--dangerously-skip-permissions")
		} else {
			args = append(args, "--permission-mode", "auto")
		}
		args = append(args, prompt)
		return runCommand("claude", args...)
	default:
		return "", usage{}, fmt.Errorf("unknown agent %q", agent)
	}
}

func runCommand(name string, args ...string) (string, usage, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := stdout.String()
	if stderr.Len() > 0 {
		out += "\n" + stderr.String()
	}
	return out, parseUsage(out), err
}

func parseUsage(output string) usage {
	var out usage
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		addUsage(&out, event["usage"])
	}
	var doc map[string]any
	if json.Unmarshal([]byte(output), &doc) == nil {
		addUsage(&out, doc["usage"])
	}
	return out
}

func addUsage(total *usage, raw any) {
	m, ok := raw.(map[string]any)
	if !ok {
		return
	}
	total.Input += jsonInt(m["input_tokens"])
	total.Output += jsonInt(m["output_tokens"])
}

func jsonInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func summarizeOutput(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return "Run completed with no final output."
	}
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil {
			if text, ok := event["text"].(string); ok && strings.TrimSpace(text) != "" {
				return trimSummary(text)
			}
			if item, ok := event["item"].(map[string]any); ok {
				if text, ok := item["text"].(string); ok && strings.TrimSpace(text) != "" {
					return trimSummary(text)
				}
			}
		}
		return trimSummary(line)
	}
	return "Run completed."
}

func trimSummary(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1800 {
		return s[:1800] + "..."
	}
	return s
}

func apiRequest[T any](ctx context.Context, method, apiURL, path, key string, body any, out *T) error {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(apiURL, "/")+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("X-Godloop-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env apiEnvelope[T]
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if env.Message != "" {
			return errors.New(env.Message)
		}
		return fmt.Errorf("request failed: %s", resp.Status)
	}
	if out != nil {
		*out = env.Data
	}
	return nil
}

func printStatus(st runnerStatus) {
	fmt.Println("AI subs")
	if len(st.Subs) == 0 {
		fmt.Println("  none")
	}
	for _, sub := range st.Subs {
		left := "usage cap unknown"
		if sub.WeeklyTokenAllowance > 0 {
			remaining := sub.WeeklyTokenAllowance - sub.EstTokensUsed
			if remaining < 0 {
				remaining = 0
			}
			left = fmt.Sprintf("%s left of %s", compact(remaining), compact(sub.WeeklyTokenAllowance))
		}
		reset := ""
		if sub.ResetAt > 0 {
			reset = " · reset " + distance(st.ServerTime, sub.ResetAt)
		}
		fmt.Printf("  #%d %s (%s): %s%s\n", sub.ID, sub.Name, sub.Type, left, reset)
	}
	fmt.Println()
	fmt.Println("Current work")
	if len(st.CurrentWork) == 0 {
		fmt.Println("  idle")
	}
	for _, work := range st.CurrentWork {
		fmt.Printf("  %s / %s: #%d %s · lease %s\n",
			work.ProjectName, work.EnvironmentName, work.TaskID, work.TaskTitle, distance(st.ServerTime, work.ClaimExpiresAt))
		if work.PromptPreview != "" {
			fmt.Println("    " + work.PromptPreview)
		}
	}
	fmt.Println()
	fmt.Println("Projects")
	if len(st.Projects) == 0 {
		fmt.Println("  none")
	}
	for _, project := range st.Projects {
		fmt.Printf("  %s  %s\n", project.ID, project.Name)
	}
}

func compact(n int64) string {
	if n >= 1_000_000 {
		return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), "0"), ".") + "M"
	}
	if n >= 1_000 {
		return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000), "0"), ".") + "k"
	}
	return strconv.FormatInt(n, 10)
}

func distance(now, target int64) string {
	d := target - now
	prefix := "in "
	if d < 0 {
		d = -d
		prefix = ""
	}
	if d < 60 {
		return prefix + strconv.FormatInt(d, 10) + "s"
	}
	if d < 3600 {
		return prefix + strconv.FormatInt(d/60, 10) + "m"
	}
	if d < 86400 {
		return prefix + strconv.FormatInt(d/3600, 10) + "h"
	}
	return prefix + strconv.FormatInt(d/86400, 10) + "d"
}

func loadConfig() (config, error) {
	path, err := configPath()
	if err != nil {
		return config{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return config{}, err
	}
	defer f.Close()
	var cfg config
	return cfg, json.NewDecoder(f).Decode(&cfg)
}

func saveConfig(cfg config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(cfg); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func configPath() (string, error) {
	if v := os.Getenv("GODLOOP_CONFIG"); v != "" {
		return v, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "godloop", "config.json"), nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func clean(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "default"
	}
	return s
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "local machine"
	}
	return name
}
