package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVersionGreater(t *testing.T) {
	if !versionGreater("0.1.1", "0.1.0") {
		t.Fatal("patch bump should be greater")
	}
	if !versionGreater("0.2.0", "0.1.9") {
		t.Fatal("minor bump should be greater")
	}
	if versionGreater("0.1.0", "0.1.0") {
		t.Fatal("same version should not be greater")
	}
	if !sameMajor("0.2.0", "0.1.0") {
		t.Fatal("same major should allow minor auto-update")
	}
	if sameMajor("1.0.0", "0.9.0") {
		t.Fatal("different major should not be same major")
	}
}

func TestCompactLoopOutput(t *testing.T) {
	raw := []byte(`{"data":{"task":{"id":7,"title":"ship","prompt":"do the thing","prompt_truncated":true},"subs":[{"name":"max","type":"claude Max","status":"active","session_tokens":10,"session_token_allowance":100,"est_tokens_used":20,"weekly_token_allowance":1000}],"next_call_seconds":900,"max_prompt_chars":12,"context_budget_hint":"task_only"}}`)
	got := compactLoopOutput(raw)
	for _, want := range []string{
		"task: #7 ship",
		"prompt:\ndo the thing",
		"prompt_truncated: true",
		"max_prompt_chars: 12",
		"context_budget_hint: task_only",
		"next_call_seconds: 900",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "{\"data\"") {
		t.Fatalf("raw JSON leaked into compact output: %s", got)
	}
}

func TestConfiguredMaxPromptChars(t *testing.T) {
	t.Setenv("GODLOOP_MAX_PROMPT_CHARS", "999999")
	if got := configuredMaxPromptChars(); got != maxPromptChars {
		t.Fatalf("configuredMaxPromptChars = %d, want cap %d", got, maxPromptChars)
	}
	t.Setenv("GODLOOP_MAX_PROMPT_CHARS", "bad")
	if got := configuredMaxPromptChars(); got != defaultMaxPromptChars {
		t.Fatalf("configuredMaxPromptChars = %d, want default %d", got, defaultMaxPromptChars)
	}
}

func TestCallLoopSendsBoundedPromptRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GODLOOP_KEY", "glp_test")
	t.Setenv("GODLOOP_PROJECT", "proj01")
	t.Setenv("GODLOOP_MAX_PROMPT_CHARS", "123")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/mcp/loop" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Godloop-Key"); got != "glp_test" {
			t.Fatalf("X-Godloop-Key = %q", got)
		}
		var body struct {
			ProjectID      string         `json:"project_id"`
			MaxPromptChars int            `json:"max_prompt_chars"`
			Report         map[string]any `json:"report"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.ProjectID != "proj01" || body.MaxPromptChars != 50 {
			t.Fatalf("body project/max = %q/%d", body.ProjectID, body.MaxPromptChars)
		}
		if body.Report["outcome"] != "progress" {
			t.Fatalf("report not forwarded: %#v", body.Report)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"task":{"id":9,"title":"wire tests","prompt":"run the thing"},"subs":[],"next_call_seconds":60,"max_prompt_chars":50,"context_budget_hint":"task_only"}}`))
	}))
	defer server.Close()
	t.Setenv("GODLOOP_URL", server.URL+"/")

	got, err := callLoop([]byte(`{"max_prompt_chars":50,"report":{"outcome":"progress"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"task: #9 wire tests",
		"prompt:\nrun the thing",
		"max_prompt_chars: 50",
		"context_budget_hint: task_only",
		"next_call_seconds: 60",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("callLoop output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"data"`) {
		t.Fatalf("raw JSON leaked into callLoop output: %s", got)
	}
}
