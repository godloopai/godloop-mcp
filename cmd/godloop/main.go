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
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	ServerTime   int64         `json:"server_time"`
	Projects     []project     `json:"projects"`
	Environments []environment `json:"environments"`
	Subs         []subUsage    `json:"subs"`
	CurrentWork  []currentWork `json:"current_work"`
}

type project struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	EnvironmentID *int64 `json:"environment_id"`
}

type environment struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type subUsage struct {
	ID                    int64  `json:"id"`
	Name                  string `json:"name"`
	Provider              string `json:"provider"`
	Type                  string `json:"type"`
	Status                string `json:"status"`
	RunnerCommand         string `json:"runner_command"`
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
	EnvironmentID   int64      `json:"environment_id"`
	Action          string     `json:"action"` // work | idle | backoff
	Reason          string     `json:"reason"`
	Task            *loopTask  `json:"task"`
	Subs            []subUsage `json:"subs"`
	NextCallSeconds int64      `json:"next_call_seconds"`
	ServerTime      int64      `json:"server_time"`
	MaxPromptChars  int        `json:"max_prompt_chars"`
	ContextHint     string     `json:"context_budget_hint"`
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

type reportPayload struct {
	EnvironmentID int64  `json:"environment_id"`
	TaskID        *int64 `json:"task_id,omitempty"`
	AISubID       *int64 `json:"ai_sub_id,omitempty"`
	Outcome       string `json:"outcome"`
	Summary       string `json:"summary"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
}

func main() {
	var err error
	if len(os.Args) < 2 {
		err = initCommand(nil)
	} else {
		switch os.Args[1] {
		case "init":
			err = initCommand(os.Args[2:])
		case "login", "connect":
			err = login(os.Args[2:])
		case "status", "usage":
			err = status(os.Args[2:])
		case "run", "daemon":
			err = runDaemon(os.Args[2:])
		case "once":
			err = once(os.Args[2:])
		case "loop":
			err = loopInteractive(os.Args[2:])
		case "logout":
			err = logout()
		default:
			usageAndExit()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usageAndExit() {
	fmt.Fprintln(os.Stderr, `usage:
  godloop
  godloop init [-api https://godloop.ai] [-workspace name]
  godloop login [-api https://godloop.ai] [-name machine]
  godloop run [-agent codex|claude] [-workdir .] [-codex-sandbox danger-full-access] [-danger]
  godloop status [-api https://godloop.ai] [-key glp_...]
  godloop usage
  godloop once -project <id> [-env name] [-sub id] [-agent codex|claude] [-workdir .] [-danger]
  godloop loop -project <id> [-workdir .] [-quiet 8s] [-danger]   # persistent interactive Claude session
  godloop logout`)
	os.Exit(2)
}

func initCommand(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	apiURL := fs.String("api", "", "godloop API URL")
	key := fs.String("key", "", "godloop runner key")
	workspace := fs.String("workspace", "", "workspace name to use or create")
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
		fmt.Println("No godloop runner login found. Opening browser login first.")
		loginArgs := []string{"-api", *apiURL, "-name", firstNonEmpty(*workspace, cfg.Machine, hostname())}
		if err := login(loginArgs); err != nil {
			return err
		}
		cfg, _ = loadConfig()
		*key = cfg.Key
		*apiURL = firstNonEmpty(cfg.APIURL, *apiURL)
		if *key == "" {
			return errors.New("login completed but no runner key was saved")
		}
	}

	var st runnerStatus
	if err := apiRequest(context.Background(), "GET", *apiURL, "/api/v1/runner/status", *key, nil, &st); err != nil {
		return err
	}
	name, err := chooseWorkspace(context.Background(), *apiURL, *key, cfg, st.Environments, *workspace, os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	cfg.APIURL = strings.TrimRight(*apiURL, "/")
	cfg.Key = *key
	cfg.Machine = name
	if err := saveConfig(cfg); err != nil {
		return err
	}
	fmt.Println("Ready. Add work in the dashboard; this machine is connected as:", name)
	fmt.Println("Keep it running with: godloop run")
	return nil
}

func chooseWorkspace(ctx context.Context, apiURL, key string, cfg config, envs []environment, requested string, in io.Reader, out io.Writer) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		if env := findEnvironmentByName(envs, requested); env != nil {
			return env.Name, nil
		}
		return createRunnerWorkspace(ctx, apiURL, key, requested)
	}
	if cfg.Machine != "" {
		if env := findEnvironmentByName(envs, cfg.Machine); env != nil {
			fmt.Fprintf(out, "Using workspace: %s\n", env.Name)
			return env.Name, nil
		}
	}
	if len(envs) == 0 {
		name := firstNonEmpty(cfg.Machine, hostname())
		fmt.Fprintf(out, "Creating workspace: %s\n", name)
		return createRunnerWorkspace(ctx, apiURL, key, name)
	}

	fmt.Fprintln(out, "Choose a workspace:")
	for i, env := range envs {
		fmt.Fprintf(out, "  %d. %s (%s)\n", i+1, env.Name, env.Kind)
	}
	fmt.Fprint(out, "Workspace number, or type a new name: ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	choice := strings.TrimSpace(line)
	if choice == "" {
		return envs[0].Name, nil
	}
	if n, err := strconv.Atoi(choice); err == nil {
		if n < 1 || n > len(envs) {
			return "", fmt.Errorf("workspace choice %d out of range", n)
		}
		return envs[n-1].Name, nil
	}
	return createRunnerWorkspace(ctx, apiURL, key, choice)
}

func findEnvironmentByName(envs []environment, name string) *environment {
	for i := range envs {
		if strings.EqualFold(strings.TrimSpace(envs[i].Name), strings.TrimSpace(name)) {
			return &envs[i]
		}
	}
	return nil
}

func createRunnerWorkspace(ctx context.Context, apiURL, key, name string) (string, error) {
	var created environment
	if err := apiRequest(ctx, "POST", apiURL, "/api/v1/runner/environments", key, map[string]string{"name": name}, &created); err != nil {
		return "", err
	}
	return created.Name, nil
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
	for {
		if time.Now().After(deadline) {
			break
		}
		var polled runnerSession
		if err := apiRequest(context.Background(), "GET", *apiURL, "/api/v1/runner/sessions/"+created.Code, "", nil, &polled); err != nil {
			return err
		}
		switch polled.Status {
		case "approved":
			if polled.Key == "" {
				continue
			}
			// the browser may have bound this runner to a chosen workspace
			ws := firstNonEmpty(polled.Name, *name)
			cfg := config{APIURL: strings.TrimRight(*apiURL, "/"), Key: polled.Key, Machine: ws}
			if err := saveConfig(cfg); err != nil {
				return err
			}
			fmt.Println("Connected workspace:", ws)
			return nil
		case "expired":
			return errors.New("pairing code expired")
		case "consumed":
			return errors.New("pairing code was already consumed")
		}
		sleep := interval
		if remaining := time.Until(deadline); remaining < sleep {
			sleep = remaining
		}
		if sleep <= 0 {
			break
		}
		time.Sleep(sleep)
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

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	apiURL := fs.String("api", "", "godloop API URL")
	key := fs.String("key", "", "godloop runner key")
	workspace := fs.String("workspace", "", "workspace name (defaults to the one you connected)")
	kind := fs.String("kind", "local", "workspace kind")
	agent := fs.String("agent", "", "codex or claude; defaults to selected sub provider, then codex")
	agentCommand := fs.String("agent-command", "", "provider command prefix; overrides the selected sub runner command")
	workdir := fs.String("workdir", ".", "repo directory")
	codexSandbox := fs.String("codex-sandbox", "danger-full-access", "Codex sandbox mode: read-only, workspace-write, or danger-full-access")
	danger := fs.Bool("danger", false, "use provider bypass/danger mode; run inside a container")
	subID := fs.Int64("sub", 0, "AI sub id to charge usage against")
	maxPromptChars := fs.Int("max-prompt-chars", 8000, "max prompt chars to request")
	progressInterval := fs.Duration("progress-interval", 20*time.Second, "how often to send live progress while an agent runs")
	poll := fs.Duration("poll", 10*time.Second, "minimum idle poll interval")
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
		fmt.Println("No godloop runner login found. Opening browser login first.")
		if err := initCommand([]string{"-api", *apiURL, "-workspace", firstNonEmpty(*workspace, cfg.Machine, hostname())}); err != nil {
			return err
		}
		cfg, _ = loadConfig()
		*key = cfg.Key
		*apiURL = firstNonEmpty(cfg.APIURL, *apiURL)
	}
	if strings.TrimSpace(*workspace) == "" {
		*workspace = firstNonEmpty(cfg.Machine, hostname())
	}
	if *poll < 2*time.Second {
		*poll = 2 * time.Second
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var reportSubID *int64
	if *subID > 0 {
		reportSubID = subID
	}
	fmt.Println("godloop runner online:", *workspace)
	for ctx.Err() == nil {
		next, worked, err := runDaemonTick(ctx, *apiURL, *key, *workspace, clean(*kind), *agent, *agentCommand, *workdir, *codexSandbox, *danger, reportSubID, *maxPromptChars, *progressInterval)
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning:", err)
			next = *poll
		}
		if worked {
			continue
		}
		if next <= 0 || next > *poll {
			next = *poll
		}
		if sleepOrDone(ctx, next) {
			return nil
		}
	}
	return nil
}

func runDaemonTick(ctx context.Context, apiURL, key, workspace, kind, agent, agentCommand, workdir, codexSandbox string, danger bool, subID *int64, maxPromptChars int, progressInterval time.Duration) (time.Duration, bool, error) {
	var st runnerStatus
	statusPath := "/api/v1/runner/status?workspace=" + url.QueryEscape(workspace) + "&kind=" + url.QueryEscape(kind)
	if err := apiRequest(ctx, "GET", apiURL, statusPath, key, nil, &st); err != nil {
		return 0, false, err
	}
	env := findEnvironmentByName(st.Environments, workspace)
	if env == nil {
		return 0, false, fmt.Errorf("workspace %q not found; run godloop init", workspace)
	}
	projects := projectsForWorkspace(st.Projects, env.ID)
	if len(projects) == 0 {
		fmt.Println("No projects assigned to this workspace yet.")
		return time.Minute, false, nil
	}

	var next time.Duration
	for _, project := range projects {
		loop, err := callProjectLoop(ctx, apiURL, key, project.ID, workspace, kind, subID, maxPromptChars)
		if err != nil {
			return 0, false, err
		}
		if loop.NextCallSeconds > 0 {
			d := time.Duration(loop.NextCallSeconds) * time.Second
			if next == 0 || d < next {
				next = d
			}
		}
		if loop.Action == "work" && loop.Task != nil {
			fmt.Printf("Running %s: #%d %s\n", project.Name, loop.Task.ID, loop.Task.Title)
			return next, true, runLoopTask(ctx, apiURL, key, loop, subID, agent, agentCommand, workdir, codexSandbox, danger, progressInterval)
		}
	}
	if next == 0 {
		next = time.Minute
	}
	return next, false, nil
}

func projectsForWorkspace(projects []project, envID int64) []project {
	out := make([]project, 0, len(projects))
	for _, project := range projects {
		if project.EnvironmentID == nil || *project.EnvironmentID == envID {
			out = append(out, project)
		}
	}
	return out
}

func callProjectLoop(ctx context.Context, apiURL, key, projectID, workspace, kind string, subID *int64, maxPromptChars int) (loopResponse, error) {
	body := map[string]any{
		"project_id":       strings.TrimSpace(projectID),
		"name":             clean(workspace),
		"kind":             clean(kind),
		"max_prompt_chars": maxPromptChars,
	}
	if subID != nil {
		body["ai_sub_id"] = *subID
	}
	var loop loopResponse
	err := apiRequest(ctx, "POST", apiURL, "/api/v1/mcp/loop", key, body, &loop)
	return loop, err
}

func runLoopTask(ctx context.Context, apiURL, key string, loop loopResponse, reportSubID *int64, agent, agentCommand, workdir, codexSandbox string, danger bool, progressInterval time.Duration) error {
	if loop.Task == nil {
		return nil
	}
	selectedSub := findSub(loop.Subs, derefInt64(reportSubID))
	if reportSubID != nil && selectedSub == nil {
		return fmt.Errorf("selected AI sub #%d was not returned by the server", *reportSubID)
	}
	agentName := cleanAgentName(agent, selectedSub)
	command := strings.TrimSpace(agentCommand)
	if command == "" && selectedSub != nil {
		command = strings.TrimSpace(selectedSub.RunnerCommand)
	}

	taskID := loop.Task.ID
	progress := func(tail string) {
		if strings.TrimSpace(tail) == "" {
			tail = "Agent is still running."
		}
		pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sendReport(pctx, apiURL, key, reportPayload{
			EnvironmentID: loop.EnvironmentID,
			TaskID:        &taskID,
			AISubID:       reportSubID,
			Outcome:       "progress",
			Summary:       summarizeOutput(tail),
		}); err != nil {
			fmt.Fprintln(os.Stderr, "warning: failed to report progress:", err)
		}
	}

	stream := startSessionStream(apiURL, key, loop.EnvironmentID, loop.Task.ProjectID, taskID, loop.Task.Title)
	defer stream.close()
	out, use, runErr := runAgent(agentName, command, workdir, loop.Task.Prompt, codexSandbox, danger, progressInterval, progress, stream.write)
	outcome := "done"
	if runErr != nil {
		outcome = "error"
	}
	summary := summarizeOutput(out)
	if err := sendReport(ctx, apiURL, key, reportPayload{
		EnvironmentID: loop.EnvironmentID,
		TaskID:        &taskID,
		AISubID:       reportSubID,
		Outcome:       outcome,
		Summary:       summary,
		InputTokens:   use.Input,
		OutputTokens:  use.Output,
	}); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	fmt.Println(summary)
	return nil
}

func once(args []string) error {
	fs := flag.NewFlagSet("once", flag.ExitOnError)
	apiURL := fs.String("api", "", "godloop API URL")
	key := fs.String("key", "", "godloop runner key")
	projectID := fs.String("project", "", "project id from godloop")
	envName := fs.String("env", "", "workspace name (defaults to the one you connected)")
	kind := fs.String("kind", "local", "environment kind")
	agent := fs.String("agent", "", "codex or claude; defaults to selected sub provider, then codex")
	agentCommand := fs.String("agent-command", "", "provider command prefix; overrides the selected sub runner command")
	workdir := fs.String("workdir", ".", "repo directory")
	codexSandbox := fs.String("codex-sandbox", "danger-full-access", "Codex sandbox mode: read-only, workspace-write, or danger-full-access")
	danger := fs.Bool("danger", false, "use provider bypass/danger mode; run inside a container")
	subID := fs.Int64("sub", 0, "AI sub id to charge usage against")
	maxPromptChars := fs.Int("max-prompt-chars", 8000, "max prompt chars to request")
	progressInterval := fs.Duration("progress-interval", 20*time.Second, "how often to send live progress while an agent runs")
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
	if strings.TrimSpace(*envName) == "" {
		*envName = firstNonEmpty(cfg.Machine, hostname())
	}

	var reportSubID *int64
	if *subID > 0 {
		reportSubID = subID
	}
	loop, err := callProjectLoop(context.Background(), *apiURL, *key, *projectID, *envName, *kind, reportSubID, *maxPromptChars)
	if err != nil {
		return err
	}
	if loop.Action != "work" || loop.Task == nil {
		if loop.Action == "" {
			loop.Action = "idle"
		}
		if loop.Reason == "" {
			loop.Reason = "no work queued"
		}
		fmt.Printf("%s - %s\n", strings.ToUpper(loop.Action), loop.Reason)
		fmt.Printf("Next check: %s\n", time.Duration(loop.NextCallSeconds)*time.Second)
		return nil
	}
	fmt.Printf("Running #%d: %s\n", loop.Task.ID, loop.Task.Title)
	if loop.Task.PromptTruncated {
		fmt.Println("Prompt was truncated by server max_prompt_chars.")
	}
	return runLoopTask(context.Background(), *apiURL, *key, loop, reportSubID, *agent, *agentCommand, *workdir, *codexSandbox, *danger, *progressInterval)
}

func sendReport(ctx context.Context, apiURL, key string, report reportPayload) error {
	var reportResp map[string]bool
	return apiRequest(ctx, "POST", apiURL, "/api/v1/mcp/report", key, report, &reportResp)
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

func findSub(subs []subUsage, id int64) *subUsage {
	if id <= 0 {
		return nil
	}
	for i := range subs {
		if subs[i].ID == id {
			return &subs[i]
		}
	}
	return nil
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func cleanAgentName(agent string, sub *subUsage) string {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent != "" {
		return agent
	}
	if sub != nil && strings.TrimSpace(sub.Provider) != "" {
		return strings.ToLower(strings.TrimSpace(sub.Provider))
	}
	return "codex"
}

// sessionStreamer mirrors the agent's terminal output to godloop so the
// dashboard can watch it live. Best-effort: failures never stop the run.
type sessionStreamer struct {
	apiURL, key, id string
	ch              chan []byte
	done            chan struct{}
}

func startSessionStream(apiURL, key string, envID int64, projectID string, taskID int64, title string) *sessionStreamer {
	var created struct {
		ID string `json:"id"`
	}
	body := map[string]any{"environment_id": envID, "project_id": projectID, "task_id": taskID, "title": title}
	if err := apiRequest(context.Background(), "POST", apiURL, "/api/v1/sessions", key, body, &created); err != nil || created.ID == "" {
		return nil
	}
	s := &sessionStreamer{apiURL: apiURL, key: key, id: created.ID, ch: make(chan []byte, 256), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for chunk := range s.ch {
			s.post("/api/v1/sessions/"+s.id+"/append", bytes.NewReader(chunk))
		}
	}()
	return s
}

func (s *sessionStreamer) post(path string, body io.Reader) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", s.apiURL+path, body)
	if err != nil {
		return
	}
	req.Header.Set("X-Godloop-Key", s.key)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

func (s *sessionStreamer) write(p []byte) {
	if s == nil {
		return
	}
	cp := append([]byte(nil), p...)
	select {
	case s.ch <- cp:
	default: // viewer backed up — drop to keep the agent unblocked
	}
}

func (s *sessionStreamer) close() {
	if s == nil {
		return
	}
	close(s.ch)
	<-s.done
	s.post("/api/v1/sessions/"+s.id+"/close", nil)
}

func runAgent(agent, command, workdir, prompt, codexSandbox string, danger bool, progressInterval time.Duration, progress func(string), stream func([]byte)) (string, usage, error) {
	switch strings.ToLower(agent) {
	case "codex":
		args := []string{"exec", "--cd", workdir, "--json"}
		if danger {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		} else {
			sandbox, err := cleanCodexSandbox(codexSandbox)
			if err != nil {
				return "", usage{}, err
			}
			args = append(args, "--sandbox", sandbox)
		}
		args = append(args, prompt)
		return runProviderCommand("codex", command, args, progressInterval, progress, stream)
	case "claude":
		args := []string{"-p", "--output-format", "json"}
		if danger {
			args = append(args, "--dangerously-skip-permissions")
		} else {
			args = append(args, "--permission-mode", "auto")
		}
		args = append(args, prompt)
		return runProviderCommand("claude", command, args, progressInterval, progress, stream)
	default:
		return "", usage{}, fmt.Errorf("unknown agent %q", agent)
	}
}

func cleanCodexSandbox(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return "danger-full-access", nil
	}
	switch mode {
	case "read-only", "workspace-write", "danger-full-access":
		return mode, nil
	default:
		return "", fmt.Errorf("unknown codex sandbox %q; use read-only, workspace-write, or danger-full-access", mode)
	}
}

func runProviderCommand(defaultName, command string, args []string, progressInterval time.Duration, progress func(string), stream func([]byte)) (string, usage, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return runCommand(defaultName, args, progressInterval, progress, stream)
	}
	return runShellCommand(command, args, progressInterval, progress, stream)
}

func runShellCommand(command string, args []string, progressInterval time.Duration, progress func(string), stream func([]byte)) (string, usage, error) {
	shell := envOrDefault("SHELL", "/bin/sh")
	shellArgs := []string{"-c", command + ` "$@"`, command}
	shellArgs = append(shellArgs, args...)
	return runCommand(shell, shellArgs, progressInterval, progress, stream)
}

func runCommand(name string, args []string, progressInterval time.Duration, progress func(string), stream func([]byte)) (string, usage, error) {
	cmd := exec.Command(name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", usage{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", usage{}, err
	}
	var mu sync.Mutex
	var full bytes.Buffer
	tail := make([]byte, 0, 8192)
	appendChunk := func(w io.Writer, p []byte) {
		_, _ = w.Write(p)
		if stream != nil {
			stream(p)
		}
		mu.Lock()
		full.Write(p)
		tail = append(tail, p...)
		if len(tail) > 8192 {
			tail = tail[len(tail)-8192:]
		}
		mu.Unlock()
	}
	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(tail)
	}
	if err := cmd.Start(); err != nil {
		return "", usage{}, err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = copyChunks(stdout, func(p []byte) { appendChunk(os.Stdout, p) })
	}()
	go func() {
		defer wg.Done()
		_, _ = copyChunks(stderr, func(p []byte) { appendChunk(os.Stderr, p) })
	}()
	done := make(chan struct{})
	if progress != nil && progressInterval > 0 {
		go func() {
			ticker := time.NewTicker(progressInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					next := snapshot()
					progress(next)
				case <-done:
					return
				}
			}
		}()
	}
	err = cmd.Wait()
	close(done)
	wg.Wait()
	out := full.String()
	return out, parseUsage(out), err
}

func copyChunks(r io.Reader, onChunk func([]byte)) (int64, error) {
	buf := make([]byte, 4096)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			onChunk(chunk)
			total += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var env apiEnvelope[T]
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			return err
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if env.Message != "" {
			return errors.New(env.Message)
		}
		if len(strings.TrimSpace(string(raw))) > 0 {
			return fmt.Errorf("request failed: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
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
		if strings.TrimSpace(sub.RunnerCommand) != "" {
			fmt.Println("    runner command configured")
		}
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
