package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestComputeBlockStart(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{
			name: "morning rolls back to 00:59",
			in:   time.Date(2026, 6, 26, 4, 20, 0, 0, time.UTC),
			want: time.Date(2026, 6, 26, 0, 59, 0, 0, time.UTC),
		},
		{
			name: "06:30 lands in 05:59 block",
			in:   time.Date(2026, 6, 26, 6, 30, 0, 0, time.UTC),
			want: time.Date(2026, 6, 26, 5, 59, 0, 0, time.UTC),
		},
		{
			name: "11:00 lands in 10:59 block",
			in:   time.Date(2026, 6, 26, 11, 0, 0, 0, time.UTC),
			want: time.Date(2026, 6, 26, 10, 59, 0, 0, time.UTC),
		},
		{
			name: "exact boundary stays put",
			in:   time.Date(2026, 6, 26, 10, 59, 0, 0, time.UTC),
			want: time.Date(2026, 6, 26, 10, 59, 0, 0, time.UTC),
		},
		{
			name: "just before midnight block crosses to prev day",
			in:   time.Date(2026, 6, 26, 0, 58, 0, 0, time.UTC),
			want: time.Date(2026, 6, 25, 20, 59, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeBlockStart(tc.in)
			if !got.Equal(tc.want) {
				t.Fatalf("computeBlockStart(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestScanSessionOutput(t *testing.T) {
	since := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := []string{
		// before since: must be excluded
		`{"timestamp":"2026-06-26T09:00:00Z","message":{"usage":{"output_tokens":100}}}`,
		// exactly at since: must be included (!Before(since))
		`{"timestamp":"2026-06-26T10:00:00Z","message":{"usage":{"output_tokens":30}}}`,
		// after since: must be included
		`{"timestamp":"2026-06-26T11:00:00Z","message":{"usage":{"output_tokens":250}}}`,
		// noise without usage: must be skipped by prefilter
		`{"timestamp":"2026-06-26T12:00:00Z","message":{"role":"user"}}`,
	}
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp jsonl: %v", err)
	}

	got := scanSessionOutput(path, since)
	const want = int64(280) // 30 (at) + 250 (after); 100 (before) excluded
	if got != want {
		t.Fatalf("scanSessionOutput = %d, want %d", got, want)
	}

	missing := scanSessionOutput(filepath.Join(t.TempDir(), "nope.jsonl"), since)
	if missing != 0 {
		t.Fatalf("scanSessionOutput(missing) = %d, want 0", missing)
	}
}

func TestSessionTokenLimit(t *testing.T) {
	const key = "GODLOOP_SESSION_TOKEN_LIMIT"
	cases := []struct {
		name  string
		set   bool
		value string
		want  int64
	}{
		{name: "unset", set: false, want: 0},
		{name: "empty", set: true, value: "", want: 0},
		{name: "whitespace", set: true, value: "   ", want: 0},
		{name: "valid", set: true, value: "5000", want: 5000},
		{name: "invalid", set: true, value: "abc", want: 0},
		{name: "negative", set: true, value: "-10", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(key, tc.value)
			} else {
				// register cleanup via Setenv, then unset for a true-absent test
				t.Setenv(key, "placeholder")
				os.Unsetenv(key)
			}
			if got := sessionTokenLimit(); got != tc.want {
				t.Fatalf("sessionTokenLimit() = %d, want %d", got, tc.want)
			}
		})
	}
}
