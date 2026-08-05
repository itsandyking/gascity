package quota

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeProbe struct {
	provider string
	snap     Snapshot
	err      error
}

func (f fakeProbe) Provider() string { return f.provider }

func (f fakeProbe) Read(context.Context) (Snapshot, error) { return f.snap, f.err }

func TestRead(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	codex := fakeProbe{
		provider: ProviderCodex,
		snap: Snapshot{
			Plan:   "prolite",
			Source: "/rollout.jsonl",
			Windows: []Window{{
				Name: "week", UsedPercent: 60, Duration: 168 * time.Hour,
				ResetsAt: now.Add(hours(71.7)), ObservedAt: now.Add(-51 * time.Minute),
			}},
		},
	}
	claude := fakeProbe{
		provider: ProviderClaude,
		snap: Snapshot{
			Plan:   "max/default_claude_max_20x",
			Source: `claude -p /usage`,
			Windows: []Window{{
				Name: "week", UsedPercent: 3, Duration: 168 * time.Hour,
				ResetsAt: now.Add(hours(163.4)), ObservedAt: now,
			}},
		},
	}

	got := Read(context.Background(), now, codex, claude)

	if !got.TakenAt.Equal(now) {
		t.Errorf("TakenAt = %v, want %v", got.TakenAt, now)
	}
	if len(got.Providers) != 2 {
		t.Fatalf("got %d providers, want 2", len(got.Providers))
	}
	// Reports sort by provider name so repeated readings print stably.
	if got.Providers[0].Provider != ProviderClaude || got.Providers[1].Provider != ProviderCodex {
		t.Fatalf("providers = %q, %q; want claude, codex",
			got.Providers[0].Provider, got.Providers[1].Provider)
	}
	if got.Providers[1].Plan != "prolite" {
		t.Errorf("codex Plan = %q, want prolite", got.Providers[1].Plan)
	}
	if got.Providers[1].Windows[0].Age != 51*time.Minute {
		t.Errorf("codex Age = %v, want 51m", got.Providers[1].Windows[0].Age)
	}
	approx(t, "codex ratio", got.Providers[1].Windows[0].Ratio, 1.1168, 0.0005)
	approx(t, "claude ratio", got.Providers[0].Windows[0].Ratio, 1.0986, 0.0005)
}

// TestReadIsolatesProbeFailures keeps one unreadable provider from blinding the
// meter to the others, and keeps a failed read from looking idle.
func TestReadIsolatesProbeFailures(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	broken := fakeProbe{
		provider: ProviderClaude,
		snap:     Snapshot{Warnings: []string{"skipped a line"}},
		err:      errors.New("claude binary not found"),
	}
	working := fakeProbe{
		provider: ProviderCodex,
		snap: Snapshot{Windows: []Window{{
			Name: "week", UsedPercent: 10, Duration: 168 * time.Hour,
			ResetsAt: now.Add(100 * time.Hour), ObservedAt: now,
		}}},
	}

	got := Read(context.Background(), now, broken, working)

	if got.Providers[0].Err == nil {
		t.Fatal("expected the failed probe to carry its error")
	}
	if len(got.Providers[0].Windows) != 0 {
		t.Errorf("failed probe reported %d windows, want none", len(got.Providers[0].Windows))
	}
	if len(got.Providers[0].Warnings) != 1 {
		t.Errorf("Warnings = %v, want the probe's warning preserved alongside the error", got.Providers[0].Warnings)
	}
	if got.Providers[1].Err != nil {
		t.Errorf("working probe reported an error: %v", got.Providers[1].Err)
	}
	if len(got.Providers[1].Windows) != 1 {
		t.Errorf("working probe reported %d windows, want 1", len(got.Providers[1].Windows))
	}
}

func TestDefaultProbes(t *testing.T) {
	probes := DefaultProbes()
	if len(probes) != 2 {
		t.Fatalf("got %d probes, want the two metered providers", len(probes))
	}
	seen := map[string]bool{}
	for _, p := range probes {
		seen[p.Provider()] = true
	}
	for _, want := range []string{ProviderClaude, ProviderCodex} {
		if !seen[want] {
			t.Errorf("DefaultProbes is missing %q", want)
		}
	}
}
