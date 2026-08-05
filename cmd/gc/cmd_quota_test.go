package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/quota"
)

type stubProbe struct {
	provider string
	snap     quota.Snapshot
	err      error
}

func (s stubProbe) Provider() string { return s.provider }

func (s stubProbe) Read(context.Context) (quota.Snapshot, error) { return s.snap, s.err }

func liveShapedReading(now time.Time) quota.Reading {
	hours := func(h float64) time.Duration { return time.Duration(h * float64(time.Hour)) }
	claude := stubProbe{
		provider: quota.ProviderClaude,
		snap: quota.Snapshot{
			Plan:   "max/default_claude_max_20x",
			Source: "claude -p /usage --output-format json",
			Windows: []quota.Window{
				{
					Name: "session", UsedPercent: 14, Duration: 5 * time.Hour,
					ResetsAt: now.Add(hours(1.1)), ObservedAt: now, Inferred: []string{"duration"},
				},
				{
					Name: "week", UsedPercent: 3, Duration: 168 * time.Hour,
					ResetsAt: now.Add(hours(163.4)), ObservedAt: now, Inferred: []string{"duration"},
				},
				{
					Name: "week", ModelScope: "Fable", UsedPercent: 0, Duration: 168 * time.Hour,
					ResetsAt: now.Add(hours(163.4)), ObservedAt: now, Inferred: []string{"duration", "resets_at"},
				},
			},
		},
	}
	codex := stubProbe{
		provider: quota.ProviderCodex,
		snap: quota.Snapshot{
			Plan:   "prolite",
			Source: "/Users/x/.codex/sessions/2026/08/04/rollout-abc.jsonl",
			Windows: []quota.Window{
				{
					Name: "week", UsedPercent: 60, Duration: 168 * time.Hour,
					ResetsAt: now.Add(hours(71.7)), ObservedAt: now.Add(-51 * time.Minute),
				},
			},
		},
	}
	return quota.Read(context.Background(), now, claude, codex)
}

func TestRenderQuotaTable(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	var out bytes.Buffer
	renderQuotaTable(&out, liveShapedReading(now))
	got := out.String()

	for _, want := range []string{
		"PROVIDER", "WINDOW", "USED%", "ELAPSED%", "RATIO", "EXHAUSTS", "RESETS", "AGE",
		"claude", "codex",
		"week (Fable)",
		"max/default_claude_max_20x",
		"prolite",
		"51m", // the Codex reading's age must be visible, not implied
	} {
		if !strings.Contains(got, want) {
			t.Errorf("table is missing %q:\n%s", want, got)
		}
	}

	// The weekly window binds for Claude even though the session window has
	// far more slack; the marker must land on the weekly row.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "claude") && strings.Contains(line, "session") {
			if strings.Contains(line, bindingMarker) {
				t.Errorf("session row must not be marked binding:\n%s", line)
			}
		}
	}
	if !strings.Contains(got, bindingMarker) {
		t.Errorf("no window marked as binding:\n%s", got)
	}
}

// TestRenderQuotaTableShowsDeadTime covers the failure mode the bead names as
// the one worth seeing: spending a window early and then idling.
func TestRenderQuotaTableShowsDeadTime(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	var out bytes.Buffer
	renderQuotaTable(&out, liveShapedReading(now))

	if !strings.Contains(out.String(), "idle") {
		t.Errorf("expected a dead-time note for the over-pace Codex window:\n%s", out.String())
	}
}

func TestRenderQuotaTableProbeFailure(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	reading := quota.Read(context.Background(), now,
		stubProbe{provider: quota.ProviderClaude, err: errors.New("exec: \"claude\": not found")},
	)
	var out bytes.Buffer
	renderQuotaTable(&out, reading)

	got := out.String()
	if !strings.Contains(got, "unreadable") {
		t.Errorf("a failed probe must read as unreadable, not idle:\n%s", got)
	}
	if !strings.Contains(got, "not found") {
		t.Errorf("a failed probe must say why:\n%s", got)
	}
}

func TestRenderQuotaJSON(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := renderQuotaJSON(&out, liveShapedReading(now)); err != nil {
		t.Fatalf("renderQuotaJSON: %v", err)
	}

	var got quotaReadingJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if len(got.Providers) != 2 {
		t.Fatalf("got %d providers, want 2", len(got.Providers))
	}
	claude := got.Providers[0]
	if claude.Provider != "claude" {
		t.Fatalf("first provider = %q, want claude", claude.Provider)
	}
	if claude.Binding == nil {
		t.Fatal("claude report carries no binding window")
	}
	if claude.Binding.Window != "week" {
		t.Errorf("binding window = %q, want week", claude.Binding.Window)
	}
	if len(claude.Windows) != 3 {
		t.Fatalf("claude has %d windows, want 3", len(claude.Windows))
	}
	if claude.Windows[0].Source == "" {
		t.Error("every window must name a source gc can point at")
	}
	// Inferred attributes travel with the number so a decision made from it can
	// name what was assumed.
	if len(claude.Windows[0].Inferred) == 0 {
		t.Error("claude session window must declare its inferred duration")
	}
	codex := got.Providers[1]
	if codex.Windows[0].AgeSeconds != 51*60 {
		t.Errorf("codex AgeSeconds = %v, want 3060", codex.Windows[0].AgeSeconds)
	}
}

func TestRenderQuotaJSONProbeFailure(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	reading := quota.Read(context.Background(), now,
		stubProbe{provider: quota.ProviderCodex, err: errors.New("no rollout files")},
	)
	var out bytes.Buffer
	if err := renderQuotaJSON(&out, reading); err != nil {
		t.Fatalf("renderQuotaJSON: %v", err)
	}
	var got quotaReadingJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got.Providers[0].Error == "" {
		t.Error("a failed probe must carry its error into the JSON reading")
	}
	if got.Providers[0].Binding != nil {
		t.Error("a failed probe must not report a binding window")
	}
}

// TestRenderQuotaTableReportsHotterSubCap covers the sub-cap-as-blocker case:
// a scoped window running hotter than the provider's shared window blocks its
// model first, and must be called out rather than left to read as slack.
func TestRenderQuotaTableReportsHotterSubCap(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	hours := func(h float64) time.Duration { return time.Duration(h * float64(time.Hour)) }
	claude := stubProbe{
		provider: quota.ProviderClaude,
		snap: quota.Snapshot{
			Windows: []quota.Window{
				{
					Name: "week", UsedPercent: 3, Duration: 168 * time.Hour,
					ResetsAt: now.Add(hours(163.4)), ObservedAt: now,
				},
				{
					Name: "week", ModelScope: "Fable", UsedPercent: 100, Duration: 168 * time.Hour,
					ResetsAt: now.Add(hours(163.4)), ObservedAt: now,
				},
			},
		},
	}
	var out bytes.Buffer
	renderQuotaTable(&out, quota.Read(context.Background(), now, claude))
	got := out.String()

	if !strings.Contains(got, "sub-cap week (Fable) is hotter (exhausted)") {
		t.Errorf("expected the exhausted Fable sub-cap called out as a blocker:\n%s", got)
	}
	// The shared weekly window still binds every other model.
	if !strings.Contains(got, "week binds") {
		t.Errorf("expected the shared weekly window to still bind:\n%s", got)
	}
}

func TestFormatSpan(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "<1m"},
		{45 * time.Minute, "45m"},
		// The decimal is the point: "5.6h of dead time" is the finding.
		{time.Duration(5.6 * float64(time.Hour)), "5.6h"},
		{72 * time.Hour, "3.0d"},
	}
	for _, tc := range tests {
		if got := formatSpan(tc.d); got != tc.want {
			t.Errorf("formatSpan(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestQuotaExitCode keeps a total blackout distinguishable from a working
// meter: every provider unreadable is a failure, one unreadable is not.
func TestQuotaExitCode(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	allBroken := quota.Read(context.Background(), now,
		stubProbe{provider: quota.ProviderClaude, err: errors.New("boom")},
		stubProbe{provider: quota.ProviderCodex, err: errors.New("boom")},
	)
	if quotaExitCode(allBroken) == 0 {
		t.Error("every provider unreadable must be a non-zero exit")
	}

	partial := quota.Read(context.Background(), now,
		stubProbe{provider: quota.ProviderClaude, err: errors.New("boom")},
		stubProbe{provider: quota.ProviderCodex, snap: quota.Snapshot{Windows: []quota.Window{{
			Name: "week", UsedPercent: 10, Duration: 168 * time.Hour,
			ResetsAt: now.Add(100 * time.Hour), ObservedAt: now,
		}}}},
	)
	if quotaExitCode(partial) != 0 {
		t.Error("one readable provider must still be a success")
	}
}
