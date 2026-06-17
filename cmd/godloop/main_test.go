package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withTempConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("GODLOOP_CONFIG", path)
	return path
}

func TestLoginStoresApprovedWorkspaceName(t *testing.T) {
	path := withTempConfig(t)
	var code string
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runner/sessions":
			code = "pair"
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"code":                  code,
				"verify_url":            "http://example.test/connect-runner?code=pair",
				"poll_interval_seconds": 0,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runner/sessions/"+code:
			polls++
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"status": "approved",
				"name":   "browser-workspace",
				"key":    "glp_secret",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if err := login([]string{"-api", server.URL, "-name", "cli-host", "-timeout", "100ms"}); err != nil {
		t.Fatal(err)
	}
	if polls == 0 {
		t.Fatal("login did not poll for approval")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Machine != "browser-workspace" {
		t.Fatalf("machine = %q, want browser-workspace", cfg.Machine)
	}
}

func TestOnceUsesSavedWorkspaceAndHonorsIdleAction(t *testing.T) {
	withTempConfig(t)
	if err := saveConfig(config{APIURL: "http://unused", Key: "glp_cfg", Machine: "saved-workspace"}); err != nil {
		t.Fatal(err)
	}
	var loopBody struct {
		Name string `json:"name"`
	}
	reported := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/mcp/loop":
			if err := json.NewDecoder(r.Body).Decode(&loopBody); err != nil {
				t.Fatal(err)
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"environment_id":      7,
				"action":              "backoff",
				"reason":              "usage limit near",
				"next_call_seconds":   900,
				"max_prompt_chars":    8000,
				"context_budget_hint": "task_only",
				"task": map[string]any{
					"id":         99,
					"project_id": "proj",
					"title":      "should not run",
					"prompt":     "do not execute",
				},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/mcp/report":
			reported = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err := once([]string{
		"-api", server.URL,
		"-project", "proj",
		"-agent", "codex",
		"-agent-command", "printf should-not-run; exit 42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if loopBody.Name != "saved-workspace" {
		t.Fatalf("loop name = %q, want saved-workspace", loopBody.Name)
	}
	if reported {
		t.Fatal("once reported a result even though the server action was backoff")
	}
}

func TestRunLoopTaskStreamsLiveSessionOutput(t *testing.T) {
	var started struct {
		EnvironmentID int64  `json:"environment_id"`
		ProjectID     string `json:"project_id"`
		TaskID        int64  `json:"task_id"`
		Title         string `json:"title"`
	}
	var appended bytes.Buffer
	closed := false
	reported := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Godloop-Key") != "glp_test" {
			t.Fatalf("runner key = %q, want glp_test", r.Header.Get("X-Godloop-Key"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions":
			if err := json.NewDecoder(r.Body).Decode(&started); err != nil {
				t.Fatal(err)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "sess1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/sess1/append":
			if _, err := io.Copy(&appended, r.Body); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/sess1/close":
			closed = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/mcp/report":
			reported = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err := runLoopTask(context.Background(), server.URL, "glp_test", loopResponse{
		EnvironmentID: 7,
		Task: &loopTask{
			ID:        42,
			ProjectID: "proj",
			Title:     "stream me",
			Prompt:    "ignored",
		},
	}, nil, "codex", "printf live-output; :", ".", "danger-full-access", false, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if started.EnvironmentID != 7 || started.ProjectID != "proj" || started.TaskID != 42 {
		t.Fatalf("live session start = %+v, want env 7 project proj task 42", started)
	}
	if !strings.Contains(started.Title, "stream me") {
		t.Fatalf("live session title = %q, want task title", started.Title)
	}
	if got := appended.String(); got != "live-output" {
		t.Fatalf("streamed output = %q, want live-output", got)
	}
	if !closed {
		t.Fatal("live session was not closed")
	}
	if !reported {
		t.Fatal("task result was not reported")
	}
}

func TestChooseWorkspaceUsesRequestedExistingWorkspace(t *testing.T) {
	var out bytes.Buffer
	name, err := chooseWorkspace(
		context.Background(),
		"http://unused",
		"glp_key",
		config{},
		[]environment{{ID: 1, Name: "laptop", Kind: "local"}},
		"laptop",
		strings.NewReader(""),
		&out,
	)
	if err != nil {
		t.Fatal(err)
	}
	if name != "laptop" {
		t.Fatalf("workspace = %q, want laptop", name)
	}
}

func TestChooseWorkspaceCreatesRequestedWorkspace(t *testing.T) {
	var gotName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/runner/environments" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotName = req.Name
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": 3, "name": req.Name, "kind": "local"}})
	}))
	defer server.Close()

	var out bytes.Buffer
	name, err := chooseWorkspace(context.Background(), server.URL, "glp_key", config{}, nil, "studio", strings.NewReader(""), &out)
	if err != nil {
		t.Fatal(err)
	}
	if name != "studio" || gotName != "studio" {
		t.Fatalf("workspace = %q, posted %q; want studio", name, gotName)
	}
}

func TestProjectsForWorkspaceIncludesAssignedAndUnassigned(t *testing.T) {
	envID := int64(7)
	otherID := int64(9)
	projects := projectsForWorkspace([]project{
		{ID: "a", Name: "assigned", EnvironmentID: &envID},
		{ID: "b", Name: "unassigned", EnvironmentID: nil},
		{ID: "c", Name: "other", EnvironmentID: &otherID},
	}, envID)
	if len(projects) != 2 || projects[0].ID != "a" || projects[1].ID != "b" {
		t.Fatalf("projects = %+v, want assigned and unassigned", projects)
	}
}

func TestParseUsageFromCodexJSONL(t *testing.T) {
	got := parseUsage(strings.Join([]string{
		`{"type":"thread.started"}`,
		`{"type":"turn.completed","usage":{"input_tokens":1200,"output_tokens":34}}`,
	}, "\n"))
	if got.Input != 1200 || got.Output != 34 {
		t.Fatalf("usage = %+v", got)
	}
}

func TestSummarizeOutput(t *testing.T) {
	got := summarizeOutput(`{"type":"item.completed","text":"done shipping"}`)
	if got != "done shipping" {
		t.Fatalf("summary = %q", got)
	}
}

func TestCompact(t *testing.T) {
	if got := compact(12500); got != "12.5k" {
		t.Fatalf("compact = %q", got)
	}
}
