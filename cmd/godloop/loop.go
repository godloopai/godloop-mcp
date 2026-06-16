package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// loopInteractive keeps a single interactive Claude Code session alive and feeds
// it work each /loop tick. Unlike `once` (headless claude -p / codex exec), the
// TUI session persists across ticks, so usage stays on the flat subscription
// instead of the metered headless credit. EXPERIMENTAL: completion is detected
// by output quiescence; tune -quiet to your setup.
func loopInteractive(args []string) error {
	fs := flag.NewFlagSet("loop", flag.ExitOnError)
	apiURL := fs.String("api", "", "godloop API URL")
	key := fs.String("key", "", "godloop runner key")
	projectID := fs.String("project", "", "project id from godloop")
	envName := fs.String("env", "", "workspace name (defaults to the one you connected)")
	kind := fs.String("kind", "local", "workspace kind")
	subID := fs.Int64("sub", 0, "AI sub id to charge usage against")
	workdir := fs.String("workdir", ".", "repo directory")
	maxPromptChars := fs.Int("max-prompt-chars", 8000, "max prompt chars to request")
	quiet := fs.Duration("quiet", 8*time.Second, "treat a task as done after this much output silence")
	maxRun := fs.Duration("max-run", 30*time.Minute, "hard cap on one task")
	danger := fs.Bool("danger", false, "skip Claude permission prompts (run in a container)")
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

	// spawn the interactive Claude TUI in a PTY so it counts as an interactive
	// session (not headless), and so we can type prompts into it across ticks.
	claudeArgs := []string{}
	if *danger {
		claudeArgs = append(claudeArgs, "--dangerously-skip-permissions")
	} else {
		claudeArgs = append(claudeArgs, "--permission-mode", "acceptEdits")
	}
	cmd := exec.Command("claude", claudeArgs...)
	cmd.Dir = *workdir
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("start interactive claude: %w", err)
	}
	defer ptmx.Close()
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	var mu sync.Mutex
	var taskBuf strings.Builder
	lastOutput := time.Now()
	var stream func([]byte)
	idle := func() time.Duration { mu.Lock(); defer mu.Unlock(); return time.Since(lastOutput) }

	// reader: PTY → operator stdout + live session stream + quiescence + capture
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				os.Stdout.Write(buf[:n])
				mu.Lock()
				lastOutput = time.Now()
				taskBuf.Write(buf[:n])
				s := stream
				mu.Unlock()
				if s != nil {
					s(append([]byte(nil), buf[:n]...))
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// let Claude finish booting before the first prompt
	waitQuiescent(ctx, idle, *quiet, 60*time.Second)

	var session *sessionStreamer
	defer func() {
		if session != nil {
			session.close()
		}
	}()

	var report *reportPayload
	for {
		if ctx.Err() != nil {
			return nil
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
		if report != nil {
			body["report"] = report
			report = nil
		}

		var loop loopResponse
		if err := apiRequest(context.Background(), "POST", *apiURL, "/api/v1/mcp/loop", *key, body, &loop); err != nil {
			fmt.Fprintln(os.Stderr, "loop call failed:", err)
			if sleepOrDone(ctx, 30*time.Second) {
				return nil
			}
			continue
		}

		// one persistent session for the whole interactive loop
		if session == nil {
			session = startSessionStream(*apiURL, *key, loop.EnvironmentID, strings.TrimSpace(*projectID), 0, "interactive claude")
			mu.Lock()
			if session != nil {
				stream = session.write
			}
			mu.Unlock()
		}

		if loop.Action == "work" && loop.Task != nil {
			fmt.Printf("\n[godloop] work — #%d %s\n", loop.Task.ID, loop.Task.Title)
			mu.Lock()
			taskBuf.Reset()
			mu.Unlock()
			ptmx.Write([]byte(loop.Task.Prompt + "\r")) // type prompt + Enter
			started := time.Now()
			for ctx.Err() == nil {
				if idle() > *quiet && time.Since(started) > 3*time.Second {
					break
				}
				if time.Since(started) > *maxRun {
					break
				}
				time.Sleep(time.Second)
			}
			mu.Lock()
			out := taskBuf.String()
			mu.Unlock()
			tid := loop.Task.ID
			report = &reportPayload{
				EnvironmentID: loop.EnvironmentID,
				TaskID:        &tid,
				Outcome:       "done",
				Summary:       summarizeOutput(out),
			}
			if *subID > 0 {
				report.AISubID = subID
			}
			continue // report this result + grab the next task immediately
		}

		next := time.Duration(loop.NextCallSeconds) * time.Second
		if next < time.Minute {
			next = time.Minute
		}
		fmt.Printf("[godloop] %s — %s (next %s)\n", strings.ToUpper(loop.Action), loop.Reason, next)
		if sleepOrDone(ctx, next) {
			return nil
		}
	}
}

func waitQuiescent(ctx context.Context, idle func() time.Duration, quiet, maxWait time.Duration) {
	start := time.Now()
	for ctx.Err() == nil && time.Since(start) < maxWait {
		if idle() > quiet {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}
