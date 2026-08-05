package quota

import (
	"strings"
	"testing"
	"time"
)

// liveClaudeUsage is verbatim output from `claude -p "/usage"` captured on
// 2026-08-04. Keeping the real text as the fixture is what makes the parser
// tests evidence rather than a restatement of the parser.
const liveClaudeUsage = `You are currently using your subscription to power your Claude Code usage

Current session: 5% used · resets Aug 5 at 3:40am (America/Los_Angeles)
Current week (all models): 5% used · resets Aug 11 at 5pm (America/Los_Angeles)
Current week (Fable): 0% used

What's contributing to your limits usage?
Approximate, based on local sessions on this machine — does not include other devices or claude.ai. Behaviors are independent characteristics, not a breakdown.

Last 24h · 2514 requests · 58 sessions
  74% of your usage was at >150k context
  35% of your usage was while 4+ sessions ran in parallel
  Top skills: /claude-api 2%

Last 7d · 25857 requests · 608 sessions
  80% of your usage was at >150k context
  Top subagents: general-purpose 17%`

func mustLA(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("no tzdata for America/Los_Angeles: %v", err)
	}
	return loc
}

func TestParseClaudeUsage(t *testing.T) {
	loc := mustLA(t)
	observed := time.Date(2026, 8, 4, 23, 45, 0, 0, loc)

	windows, err := parseClaudeUsage(liveClaudeUsage, observed)
	if err != nil {
		t.Fatalf("parseClaudeUsage: %v", err)
	}
	if len(windows) != 3 {
		t.Fatalf("got %d windows, want 3: %+v", len(windows), windows)
	}

	wantResetSession := time.Date(2026, 8, 5, 3, 40, 0, 0, loc)
	wantResetWeek := time.Date(2026, 8, 11, 17, 0, 0, 0, loc)

	tests := []struct {
		window    Window
		name      string
		scope     string
		used      float64
		duration  time.Duration
		reset     time.Time
		wantInfer []string
	}{
		{windows[0], "session", "", 5, ClaudeSessionWindow, wantResetSession, []string{"duration"}},
		{windows[1], "week", "", 5, ClaudeWeeklyWindow, wantResetWeek, []string{"duration"}},
		// The Fable line states no reset; it is a sub-cap of the weekly budget
		// and inherits that window's boundaries, which must be declared.
		{windows[2], "week", "Fable", 0, ClaudeWeeklyWindow, wantResetWeek, []string{"duration", "resets_at"}},
	}

	for _, tc := range tests {
		w := tc.window
		label := w.Name + "/" + w.ModelScope
		if w.Name != tc.name || w.ModelScope != tc.scope {
			t.Errorf("%s: name/scope = %q/%q, want %q/%q", label, w.Name, w.ModelScope, tc.name, tc.scope)
		}
		if w.UsedPercent != tc.used {
			t.Errorf("%s: UsedPercent = %v, want %v", label, w.UsedPercent, tc.used)
		}
		if w.Duration != tc.duration {
			t.Errorf("%s: Duration = %v, want %v", label, w.Duration, tc.duration)
		}
		if !w.ResetsAt.Equal(tc.reset) {
			t.Errorf("%s: ResetsAt = %v, want %v", label, w.ResetsAt, tc.reset)
		}
		if !w.ObservedAt.Equal(observed) {
			t.Errorf("%s: ObservedAt = %v, want %v", label, w.ObservedAt, observed)
		}
		for _, want := range tc.wantInfer {
			if !hasString(w.Inferred, want) {
				t.Errorf("%s: Inferred = %v, want it to include %q", label, w.Inferred, want)
			}
		}
	}
}

// TestParseClaudeUsageHyphenSeparator covers the separator variant recorded
// earlier on this bead; the CLI has printed both "·" and "-".
func TestParseClaudeUsageHyphenSeparator(t *testing.T) {
	loc := mustLA(t)
	observed := time.Date(2026, 8, 4, 21, 0, 0, 0, loc)
	text := "Current session: 14% used - resets Aug 4 at 10:40pm (America/Los_Angeles)"

	windows, err := parseClaudeUsage(text, observed)
	if err != nil {
		t.Fatalf("parseClaudeUsage: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(windows))
	}
	if windows[0].UsedPercent != 14 {
		t.Errorf("UsedPercent = %v, want 14", windows[0].UsedPercent)
	}
	want := time.Date(2026, 8, 4, 22, 40, 0, 0, loc)
	if !windows[0].ResetsAt.Equal(want) {
		t.Errorf("ResetsAt = %v, want %v", windows[0].ResetsAt, want)
	}
}

func TestParseClaudeUsageFractionalPercent(t *testing.T) {
	loc := mustLA(t)
	observed := time.Date(2026, 8, 4, 21, 0, 0, 0, loc)
	text := "Current week (all models): 12.5% used · resets Aug 11 at 5pm (America/Los_Angeles)"

	windows, err := parseClaudeUsage(text, observed)
	if err != nil {
		t.Fatalf("parseClaudeUsage: %v", err)
	}
	if windows[0].UsedPercent != 12.5 {
		t.Errorf("UsedPercent = %v, want 12.5", windows[0].UsedPercent)
	}
}

// TestParseClaudeUsageYearRollover covers the missing year in the reset text:
// a reset in early January read on New Year's Eve belongs to the next year.
func TestParseClaudeUsageYearRollover(t *testing.T) {
	loc := mustLA(t)
	observed := time.Date(2026, 12, 31, 20, 0, 0, 0, loc)
	text := "Current week (all models): 40% used · resets Jan 2 at 5pm (America/Los_Angeles)"

	windows, err := parseClaudeUsage(text, observed)
	if err != nil {
		t.Fatalf("parseClaudeUsage: %v", err)
	}
	want := time.Date(2027, 1, 2, 17, 0, 0, 0, loc)
	if !windows[0].ResetsAt.Equal(want) {
		t.Errorf("ResetsAt = %v, want %v", windows[0].ResetsAt, want)
	}
}

// TestParseClaudeUsageNoWindows fails loudly rather than reporting an empty —
// and therefore idle-looking — meter when the output shape changes.
func TestParseClaudeUsageNoWindows(t *testing.T) {
	_, err := parseClaudeUsage("Something else entirely.\nNo usage lines here.", time.Now())
	if err == nil {
		t.Fatal("expected an error when no usage lines are present")
	}
	if !strings.Contains(err.Error(), "no usage windows") {
		t.Errorf("error = %q, want it to name the missing windows", err)
	}
}

// TestParseClaudeUsageUnparseableReset keeps a window whose reset text cannot
// be read out of the meter entirely, rather than admitting it with a zero
// reset that would read as "expired".
func TestParseClaudeUsageUnparseableReset(t *testing.T) {
	observed := time.Now()
	text := "Current session: 9% used · resets sometime soon (Mars/Olympus_Mons)"

	_, err := parseClaudeUsage(text, observed)
	if err == nil {
		t.Fatal("expected an error for an unparseable reset time")
	}
}

func TestExtractClaudeResultJSON(t *testing.T) {
	raw := `{"is_error":false,"result":"Current session: 5% used · resets Aug 5 at 3:40am (America/Los_Angeles)","type":"result"}`
	got, err := extractClaudeResult([]byte(raw))
	if err != nil {
		t.Fatalf("extractClaudeResult: %v", err)
	}
	if !strings.HasPrefix(got, "Current session: 5% used") {
		t.Errorf("result = %q, want the usage text", got)
	}
}

func TestExtractClaudeResultReportsProviderError(t *testing.T) {
	raw := `{"is_error":true,"result":"Credit balance too low","type":"result"}`
	if _, err := extractClaudeResult([]byte(raw)); err == nil {
		t.Fatal("expected an error when the provider flags is_error")
	}
}

func TestExtractClaudeResultRejectsNonJSON(t *testing.T) {
	if _, err := extractClaudeResult([]byte("not json at all")); err == nil {
		t.Fatal("expected an error for a non-JSON envelope")
	}
}

func TestParseClaudePlan(t *testing.T) {
	raw := `{"claudeAiOauth":{"accessToken":"secret","subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`
	if got := parseClaudePlan([]byte(raw)); got != "max/default_claude_max_20x" {
		t.Errorf("parseClaudePlan = %q, want %q", got, "max/default_claude_max_20x")
	}
	if got := parseClaudePlan([]byte(`{"claudeAiOauth":{"subscriptionType":"pro"}}`)); got != "pro" {
		t.Errorf("parseClaudePlan = %q, want %q", got, "pro")
	}
	if got := parseClaudePlan([]byte("{}")); got != "" {
		t.Errorf("parseClaudePlan on an empty object = %q, want empty", got)
	}
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
