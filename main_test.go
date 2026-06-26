package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestComputeBlockStart(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"morning", time.Date(2026, 6, 26, 4, 20, 0, 0, time.UTC), time.Date(2026, 6, 26, 0, 59, 0, 0, time.UTC)},
		{"mid", time.Date(2026, 6, 26, 6, 30, 0, 0, time.UTC), time.Date(2026, 6, 26, 5, 59, 0, 0, time.UTC)},
		{"top-of-hour", time.Date(2026, 6, 26, 11, 0, 0, 0, time.UTC), time.Date(2026, 6, 26, 10, 59, 0, 0, time.UTC)},
		{"exact-boundary", time.Date(2026, 6, 26, 10, 59, 0, 0, time.UTC), time.Date(2026, 6, 26, 10, 59, 0, 0, time.UTC)},
		{"just-before-rolls-back-a-day", time.Date(2026, 6, 26, 0, 58, 0, 0, time.UTC), time.Date(2026, 6, 25, 20, 59, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		if got := computeBlockStart(tc.in); !got.Equal(tc.want) {
			t.Errorf("%s: computeBlockStart(%s) = %s, want %s", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestScanSessionOutput(t *testing.T) {
	since := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	body := strings.Join([]string{
		`{"timestamp":"2026-06-26T09:00:00Z","message":{"usage":{"output_tokens":100,"input_tokens":7}}}`, // before since — excluded
		`{"timestamp":"2026-06-26T10:00:00Z","message":{"usage":{"output_tokens":5}}}`,                    // exactly at since — included
		`{"timestamp":"2026-06-26T11:30:00Z","message":{"usage":{"output_tokens":42,"input_tokens":9}}}`,  // after since — included
		`{"no":"usage here, line ignored"}`,
	}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := scanSessionOutput(path, since); got != 47 {
		t.Fatalf("scanSessionOutput = %d, want 47 (only output_tokens at/after since)", got)
	}
	if got := scanSessionOutput(filepath.Join(t.TempDir(), "missing.jsonl"), since); got != 0 {
		t.Fatalf("scanSessionOutput on missing file = %d, want 0", got)
	}
}

func TestSessionTokenLimit(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want int64
	}{
		{"unset", false, "", 0},
		{"empty", true, "", 0},
		{"valid", true, "150000", 150000},
		{"whitespace", true, "  150000  ", 150000},
		{"invalid", true, "abc", 0},
		{"negative", true, "-5", 0},
	}
	for _, tc := range cases {
		if tc.set {
			t.Setenv("GODLOOP_SESSION_TOKEN_LIMIT", tc.val)
		} else {
			os.Unsetenv("GODLOOP_SESSION_TOKEN_LIMIT")
		}
		if got := sessionTokenLimit(); got != tc.want {
			t.Errorf("%s: sessionTokenLimit() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

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
