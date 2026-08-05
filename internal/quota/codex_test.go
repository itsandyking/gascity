package quota

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// liveCodexLine is a verbatim rollout entry captured from
// ~/.codex/sessions/2026/08/04/ on 2026-08-04. resets_at is epoch seconds.
const liveCodexLine = `{"timestamp":"2026-08-05T06:30:21.990Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":25744,"cached_input_tokens":9984,"cache_write_input_tokens":0,"output_tokens":1472,"reasoning_output_tokens":170,"total_tokens":27216},"model_context_window":258400},"rate_limits":{"limit_id":"codex","limit_name":null,"primary":{"used_percent":60.0,"window_minutes":10080,"resets_at":1786162483},"secondary":null,"credits":{"has_credits":false,"unlimited":false,"balance":"0"},"individual_limit":null,"spend_control_reached":null,"plan_type":"prolite","rate_limit_reached_type":null}}}`

func TestParseCodexReading(t *testing.T) {
	got, ok, err := parseCodexReading([]byte(liveCodexLine))
	if err != nil {
		t.Fatalf("parseCodexReading: %v", err)
	}
	if !ok {
		t.Fatal("expected the line to carry a reading")
	}
	if got.plan != "prolite" {
		t.Errorf("plan = %q, want %q", got.plan, "prolite")
	}
	wantObserved := time.Date(2026, 8, 5, 6, 30, 21, 990000000, time.UTC)
	if !got.observedAt.Equal(wantObserved) {
		t.Errorf("observedAt = %v, want %v", got.observedAt, wantObserved)
	}
	if len(got.windows) != 1 {
		t.Fatalf("got %d windows, want 1: %+v", len(got.windows), got.windows)
	}

	w := got.windows[0]
	if w.Name != "week" {
		t.Errorf("Name = %q, want %q", w.Name, "week")
	}
	if w.ModelScope != "" {
		t.Errorf("ModelScope = %q, want empty (Codex states no per-model sub-cap)", w.ModelScope)
	}
	if w.UsedPercent != 60 {
		t.Errorf("UsedPercent = %v, want 60", w.UsedPercent)
	}
	if w.Duration != 168*time.Hour {
		t.Errorf("Duration = %v, want 168h", w.Duration)
	}
	if !w.ResetsAt.Equal(time.Unix(1786162483, 0)) {
		t.Errorf("ResetsAt = %v, want %v", w.ResetsAt, time.Unix(1786162483, 0))
	}
	if !w.ObservedAt.Equal(wantObserved) {
		t.Errorf("ObservedAt = %v, want %v", w.ObservedAt, wantObserved)
	}
	// Codex states both the window length and the reset instant, so nothing
	// about this window is gascity's guess.
	if len(w.Inferred) != 0 {
		t.Errorf("Inferred = %v, want nothing inferred", w.Inferred)
	}
}

func TestParseCodexReadingSecondaryWindow(t *testing.T) {
	line := `{"timestamp":"2026-08-05T06:30:21.990Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":12.0,"window_minutes":300,"resets_at":1786162483},"secondary":{"used_percent":60.0,"window_minutes":10080,"resets_at":1786500000},"plan_type":"prolite"}}}`

	got, ok, err := parseCodexReading([]byte(line))
	if err != nil {
		t.Fatalf("parseCodexReading: %v", err)
	}
	if !ok {
		t.Fatal("expected a reading")
	}
	if len(got.windows) != 2 {
		t.Fatalf("got %d windows, want 2", len(got.windows))
	}
	if got.windows[0].Name != "session" || got.windows[0].Duration != 5*time.Hour {
		t.Errorf("primary = %q/%v, want session/5h", got.windows[0].Name, got.windows[0].Duration)
	}
	if got.windows[1].Name != "week" {
		t.Errorf("secondary = %q, want week", got.windows[1].Name)
	}
}

func TestParseCodexReadingSkipsUnrelatedLines(t *testing.T) {
	for _, line := range []string{
		`{"timestamp":"2026-08-05T06:30:21.990Z","type":"response_item","payload":{"type":"message"}}`,
		`{"timestamp":"2026-08-05T06:30:21.990Z","type":"event_msg","payload":{"type":"token_count","rate_limits":null}}`,
	} {
		_, ok, err := parseCodexReading([]byte(line))
		if err != nil {
			t.Fatalf("parseCodexReading(%q): %v", line, err)
		}
		if ok {
			t.Errorf("line %q should not yield a reading", line)
		}
	}
}

// TestParseCodexReadingMalformedJSON keeps a corrupt rate-limit line loud
// rather than silently reporting "no reading found", which would look like an
// idle meter.
func TestParseCodexReadingMalformedJSON(t *testing.T) {
	if _, _, err := parseCodexReading([]byte(`{"payload":{"rate_limits":{ broken`)); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

// TestReadCodexRolloutWarnsOnTruncatedLine covers the file that matters most:
// the freshest rollout is being appended to right now, so its last line can be
// half-written. That must cost a warning, not the whole reading.
func TestReadCodexRolloutWarnsOnTruncatedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-live.jsonl")
	body := liveCodexLine + "\n" + `{"timestamp":"2026-08-05T06:31:00.000Z","payload":{"rate_limits":{"prim` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, warnings, err := readCodexRollout(path)
	if err != nil {
		t.Fatalf("readCodexRollout: %v", err)
	}
	if !ok {
		t.Fatal("expected the complete reading to survive a truncated trailing line")
	}
	if got.windows[0].UsedPercent != 60 {
		t.Errorf("UsedPercent = %v, want 60", got.windows[0].UsedPercent)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	if !strings.Contains(warnings[0], "line 2") {
		t.Errorf("warning = %q, want it to name the offending line", warnings[0])
	}
}

func TestCodexWindowName(t *testing.T) {
	tests := []struct {
		minutes int
		want    string
	}{
		{10080, "week"},
		{1440, "day"},
		{300, "session"},
		{60, "1h"},
		{45, "45m"},
		{0, "window"},
	}
	for _, tc := range tests {
		if got := codexWindowName(tc.minutes); got != tc.want {
			t.Errorf("codexWindowName(%d) = %q, want %q", tc.minutes, got, tc.want)
		}
	}
}

func TestReadCodexRolloutKeepsLatestReading(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-a.jsonl")
	lines := []string{
		`{"timestamp":"2026-08-05T05:00:00.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":40.0,"window_minutes":10080,"resets_at":1786162483},"plan_type":"prolite"}}}`,
		`{"timestamp":"2026-08-05T05:30:00.000Z","type":"response_item","payload":{"type":"message"}}`,
		`{"timestamp":"2026-08-05T06:00:00.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":41.0,"window_minutes":10080,"resets_at":1786162483},"plan_type":"prolite"}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, _, err := readCodexRollout(path)
	if err != nil {
		t.Fatalf("readCodexRollout: %v", err)
	}
	if !ok {
		t.Fatal("expected a reading")
	}
	if got.windows[0].UsedPercent != 41 {
		t.Errorf("UsedPercent = %v, want the last reading in the file (41)", got.windows[0].UsedPercent)
	}
}

// TestReadCodexRolloutSkipsOversizedLines proves an interleaved multi-megabyte
// message payload does not hide the small telemetry line that follows it.
func TestReadCodexRolloutSkipsOversizedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-big.jsonl")
	huge := `{"type":"response_item","payload":{"text":"` + strings.Repeat("x", 2*1024*1024) + `"}}`
	body := huge + "\n" + liveCodexLine + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, _, err := readCodexRollout(path)
	if err != nil {
		t.Fatalf("readCodexRollout: %v", err)
	}
	if !ok {
		t.Fatal("expected the small reading after the oversized line")
	}
	if got.windows[0].UsedPercent != 60 {
		t.Errorf("UsedPercent = %v, want 60", got.windows[0].UsedPercent)
	}
}

func TestReadCodexRolloutNoReading(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-empty.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"response_item"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, _, err := readCodexRollout(path)
	if err != nil {
		t.Fatalf("readCodexRollout: %v", err)
	}
	if ok {
		t.Error("expected no reading from a rollout with no rate_limits entries")
	}
}

func TestCodexProbeReadPicksFreshestReading(t *testing.T) {
	home := t.TempDir()
	day := filepath.Join(home, ".codex", "sessions", "2026", "08", "04")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, ts string, used float64, mtime time.Time) {
		t.Helper()
		line := `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":` +
			strconv.FormatFloat(used, 'f', -1, 64) + `,"window_minutes":10080,"resets_at":1786162483},"plan_type":"prolite"}}}`
		p := filepath.Join(day, name)
		if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	write("rollout-old.jsonl", "2026-08-04T10:00:00.000Z", 30, base)
	write("rollout-new.jsonl", "2026-08-04T11:00:00.000Z", 55, base.Add(time.Hour))

	p := NewCodexProbe()
	p.home = home
	snap, err := p.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if snap.Plan != "prolite" {
		t.Errorf("Plan = %q, want prolite", snap.Plan)
	}
	if len(snap.Windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(snap.Windows))
	}
	if snap.Windows[0].UsedPercent != 55 {
		t.Errorf("UsedPercent = %v, want the freshest reading (55)", snap.Windows[0].UsedPercent)
	}
	if !strings.Contains(snap.Source, "rollout-new.jsonl") {
		t.Errorf("Source = %q, want it to name the file the reading came from", snap.Source)
	}
}

func TestCodexProbeReadNoSessions(t *testing.T) {
	p := NewCodexProbe()
	p.home = t.TempDir()
	if _, err := p.Read(context.Background()); err == nil {
		t.Fatal("expected an error when no rollout files exist")
	}
}
