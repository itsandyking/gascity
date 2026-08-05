package quota

import (
	"math"
	"testing"
	"time"
)

// hours builds a duration from a fractional hour count. A plain
// time.Duration(1.1 * float64(time.Hour)) does not compile for an untyped
// constant, and these fixtures come from live readings measured in hours.
func hours(h float64) time.Duration {
	return time.Duration(h * float64(time.Hour))
}

func approx(t *testing.T, label string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.5f, want %.5f (tol %.5f)", label, got, want, tol)
	}
}

// TestPaceAtCodexWeekly reproduces the live Codex reading recorded on ga-o2j1
// (60%% used, 96.3h into a 168h window) so the arithmetic stays pinned to a
// real observation rather than to a synthetic example.
func TestPaceAtCodexWeekly(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	w := Window{
		Name:        "week",
		UsedPercent: 60,
		Duration:    168 * time.Hour,
		ResetsAt:    now.Add(hours(71.7)),
		ObservedAt:  now.Add(-10 * time.Minute),
	}

	p := w.PaceAt(now)

	if p.State != RatioOK {
		t.Fatalf("State = %q, want %q", p.State, RatioOK)
	}
	approx(t, "Elapsed hours", p.Elapsed.Hours(), 96.3, 0.01)
	approx(t, "ElapsedPercent", p.ElapsedPercent, 57.32, 0.01)
	approx(t, "BurnPercentPerHour", p.BurnPercentPerHour, 0.6231, 0.0005)
	approx(t, "AllowedPercentPerHour", p.AllowedPercentPerHour, 0.5579, 0.0005)
	approx(t, "Ratio", p.Ratio, 1.1168, 0.0005)
	approx(t, "Confidence", p.Confidence, 0.5732, 0.0005)
	if p.Age != 10*time.Minute {
		t.Errorf("Age = %v, want 10m", p.Age)
	}

	// Straight-line projection: 40%% left at 0.6231%%/h exhausts 64.2h out,
	// 7.5h before the window resets — the dead-time failure mode the bead
	// names as the one worth seeing.
	wantExhaust := now.Add(hours(64.20))
	if d := p.ExhaustsAt.Sub(wantExhaust); d > time.Minute || d < -time.Minute {
		t.Errorf("ExhaustsAt = %v, want ~%v", p.ExhaustsAt, wantExhaust)
	}
	approx(t, "DeadTime hours", p.DeadTime().Hours(), 7.5, 0.02)
}

// TestPaceAtClaudeWindows covers the two live Claude windows from the same
// reading: a weekly window running hot and a 5h session window with slack.
func TestPaceAtClaudeWindows(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)

	week := Window{
		Name:        "week",
		UsedPercent: 3,
		Duration:    168 * time.Hour,
		ResetsAt:    now.Add(hours(163.4)),
		ObservedAt:  now,
	}.PaceAt(now)
	approx(t, "week burn", week.BurnPercentPerHour, 0.6522, 0.0005)
	approx(t, "week allowed", week.AllowedPercentPerHour, 0.5936, 0.0005)
	approx(t, "week ratio", week.Ratio, 1.0986, 0.0005)

	session := Window{
		Name:        "session",
		UsedPercent: 14,
		Duration:    5 * time.Hour,
		ResetsAt:    now.Add(hours(1.1)),
		ObservedAt:  now,
	}.PaceAt(now)
	approx(t, "session burn", session.BurnPercentPerHour, 3.5897, 0.0005)
	approx(t, "session ratio", session.Ratio, 0.0459, 0.0005)

	// Pacing to the session window alone would have said "run 4x harder" and
	// been wrong. The weekly window binds.
	if week.Ratio <= session.Ratio {
		t.Fatalf("expected weekly (%.3f) to bind over session (%.3f)", week.Ratio, session.Ratio)
	}
}

func TestPaceAtEdgeStates(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)

	tests := []struct {
		name      string
		window    Window
		wantState RatioState
	}{
		{
			name: "cold start: no time elapsed yet",
			window: Window{
				Name: "week", UsedPercent: 0, Duration: 168 * time.Hour,
				ResetsAt: now.Add(168 * time.Hour), ObservedAt: now,
			},
			wantState: RatioColdStart,
		},
		{
			name: "exhausted: no allowance left to pace",
			window: Window{
				Name: "week", UsedPercent: 100, Duration: 168 * time.Hour,
				ResetsAt: now.Add(24 * time.Hour), ObservedAt: now,
			},
			wantState: RatioExhausted,
		},
		{
			name: "expired: the observed window already reset",
			window: Window{
				Name: "session", UsedPercent: 40, Duration: 5 * time.Hour,
				ResetsAt: now.Add(-1 * time.Minute), ObservedAt: now.Add(-time.Hour),
			},
			wantState: RatioExpired,
		},
		{
			name: "unusable: provider stated no window length",
			window: Window{
				Name: "week", UsedPercent: 40,
				ResetsAt: now.Add(24 * time.Hour), ObservedAt: now,
			},
			wantState: RatioUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.window.PaceAt(now)
			if p.State != tc.wantState {
				t.Fatalf("State = %q, want %q", p.State, tc.wantState)
			}
			if p.State != RatioOK && p.Ratio != 0 {
				t.Errorf("Ratio = %v, want 0 for non-OK state", p.Ratio)
			}
		})
	}
}

// TestPaceAtIdleWindowNeverExhausts guards the under-spend direction: zero burn
// is a valid pace, not an error, and it must not project an exhaustion time.
func TestPaceAtIdleWindowNeverExhausts(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	p := Window{
		Name: "week", UsedPercent: 0, Duration: 168 * time.Hour,
		ResetsAt: now.Add(100 * time.Hour), ObservedAt: now,
	}.PaceAt(now)

	if p.State != RatioOK {
		t.Fatalf("State = %q, want %q", p.State, RatioOK)
	}
	if p.Ratio != 0 {
		t.Errorf("Ratio = %v, want 0", p.Ratio)
	}
	if !p.ExhaustsAt.IsZero() {
		t.Errorf("ExhaustsAt = %v, want zero (never exhausts at zero burn)", p.ExhaustsAt)
	}
	if p.DeadTime() != 0 {
		t.Errorf("DeadTime = %v, want 0", p.DeadTime())
	}
}

// TestPaceAtNeverReportsNegativeAge covers a reading stamped after the
// evaluation instant — which happens on every run, because the meter captures
// "now" before the probes go out to the providers.
func TestPaceAtNeverReportsNegativeAge(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	p := Window{
		Name: "week", UsedPercent: 5, Duration: 168 * time.Hour,
		ResetsAt: now.Add(100 * time.Hour), ObservedAt: now.Add(4 * time.Second),
	}.PaceAt(now)

	if p.Age != 0 {
		t.Errorf("Age = %v, want 0 (a reading cannot be younger than nothing)", p.Age)
	}
}

// TestPaceAtClampsElapsedToWindow covers a reading whose reset is further out
// than the stated window length (clock skew or a provider re-anchor): elapsed
// must never go negative and turn burn rate into nonsense.
func TestPaceAtClampsElapsedToWindow(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	p := Window{
		Name: "week", UsedPercent: 5, Duration: 168 * time.Hour,
		ResetsAt: now.Add(200 * time.Hour), ObservedAt: now,
	}.PaceAt(now)

	if p.Elapsed < 0 {
		t.Fatalf("Elapsed = %v, want >= 0", p.Elapsed)
	}
	if p.State != RatioColdStart {
		t.Fatalf("State = %q, want %q", p.State, RatioColdStart)
	}
}

func TestWindowGates(t *testing.T) {
	unscoped := Window{Name: "week"}
	fable := Window{Name: "week", ModelScope: "Fable"}

	tests := []struct {
		window Window
		model  string
		want   bool
	}{
		{unscoped, "claude-opus-5", true},
		{unscoped, "claude-fable-5", true},
		{unscoped, "", true},
		{fable, "claude-fable-5", true},
		{fable, "Claude-Fable-5", true},
		{fable, "claude-opus-5", false},
		{fable, "", false},
	}

	for _, tc := range tests {
		if got := tc.window.Gates(tc.model); got != tc.want {
			t.Errorf("Window{scope:%q}.Gates(%q) = %v, want %v", tc.window.ModelScope, tc.model, got, tc.want)
		}
	}
}

// TestReportBinding covers the conjunctive-gating rule: within a provider a
// request is servable only if every window that gates it has headroom, so the
// binding window is the hottest of those — never the slackest.
func TestReportBinding(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	mk := func(name, scope string, used float64, left time.Duration, dur time.Duration) Pace {
		return Window{
			Name: name, ModelScope: scope, UsedPercent: used,
			Duration: dur, ResetsAt: now.Add(left), ObservedAt: now,
		}.PaceAt(now)
	}

	r := Report{
		Provider: "claude",
		Windows: []Pace{
			mk("session", "", 14, hours(1.1), 5*time.Hour),      // ratio 0.05
			mk("week", "", 3, hours(163.4), 168*time.Hour),      // ratio 1.10
			mk("week", "Fable", 0, hours(163.4), 168*time.Hour), // ratio 0.00
		},
	}

	b, ok := r.Binding("")
	if !ok {
		t.Fatal("Binding(\"\") returned no window")
	}
	if b.Name != "week" || b.ModelScope != "" {
		t.Errorf("Binding(\"\") = %q/%q, want week/(unscoped)", b.Name, b.ModelScope)
	}

	// A Fable sub-cap reading 0%% is NOT free capacity: the shared weekly
	// window still gates the request and still binds.
	b, ok = r.Binding("claude-fable-5")
	if !ok {
		t.Fatal("Binding(fable) returned no window")
	}
	if b.Name != "week" || b.ModelScope != "" {
		t.Errorf("Binding(fable) = %q/%q, want the shared weekly window", b.Name, b.ModelScope)
	}

	// An exhausted sub-cap blocks its own model even with weekly headroom, and
	// leaves other models governed by the shared window.
	r.Windows[2] = mk("week", "Fable", 100, hours(163.4), 168*time.Hour)
	b, _ = r.Binding("claude-fable-5")
	if b.ModelScope != "Fable" || b.State != RatioExhausted {
		t.Errorf("Binding(fable) = %q/%q, want the exhausted Fable sub-cap", b.ModelScope, b.State)
	}
	b, _ = r.Binding("claude-opus-5")
	if b.ModelScope != "" || b.Name != "week" {
		t.Errorf("Binding(opus) = %q/%q, want the shared weekly window", b.Name, b.ModelScope)
	}
}

// TestReportBindingPrefersFreshOverExpired keeps a stale window that already
// reset from masquerading as the binding constraint.
func TestReportBindingPrefersFreshOverExpired(t *testing.T) {
	now := time.Date(2026, 8, 4, 21, 34, 0, 0, time.UTC)
	expired := Window{
		Name: "session", UsedPercent: 99, Duration: 5 * time.Hour,
		ResetsAt: now.Add(-time.Hour), ObservedAt: now.Add(-6 * time.Hour),
	}.PaceAt(now)
	live := Window{
		Name: "week", UsedPercent: 50, Duration: 168 * time.Hour,
		ResetsAt: now.Add(84 * time.Hour), ObservedAt: now,
	}.PaceAt(now)

	r := Report{Provider: "claude", Windows: []Pace{expired, live}}
	b, ok := r.Binding("")
	if !ok {
		t.Fatal("Binding returned no window")
	}
	if b.Name != "week" {
		t.Errorf("Binding = %q, want the live weekly window over the expired session", b.Name)
	}
}

func TestReportBindingEmpty(t *testing.T) {
	if _, ok := (Report{Provider: "codex"}).Binding(""); ok {
		t.Error("Binding on a report with no windows should report no binding window")
	}
}
