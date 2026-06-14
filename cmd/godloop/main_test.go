package main

import (
	"strings"
	"testing"
)

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
