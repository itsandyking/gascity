package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Claude window lengths. The CLI states a reset time but never a window
// length, so these are gascity's inference and every window carrying one
// declares "duration" in [Window.Inferred]. Both are cross-checked against
// live readings: a session reset 3.9h out at 14% used, and a weekly reset
// 163.4h out, are only consistent with these lengths.
const (
	// ClaudeSessionWindow is the rolling session allowance window.
	ClaudeSessionWindow = 5 * time.Hour
	// ClaudeWeeklyWindow is the weekly allowance window.
	ClaudeWeeklyWindow = 7 * 24 * time.Hour
)

// claudeProbeTimeout bounds the probe exec. The observed call returns in under
// a second; the bound exists so a hung CLI cannot stall the meter.
const claudeProbeTimeout = 60 * time.Second

// claudeUsageLine matches one allowance line of `claude -p "/usage"`, e.g.
//
//	Current week (all models): 5% used · resets Aug 11 at 5pm (America/Los_Angeles)
//
// The reset clause is optional: a sub-cap line states only its percentage.
// Both "·" and "-" have been observed as the separator.
var claudeUsageLine = regexp.MustCompile(
	`^Current\s+(.+?):\s*([0-9]+(?:\.[0-9]+)?)%\s+used\s*(?:[·\-–—]\s*resets\s+(.+?))?\s*$`)

// trailingParenthetical splits a string into its body and a trailing
// parenthesised qualifier. It serves both label forms the CLI prints:
// "week (all models)" and "Aug 11 at 5pm (America/Los_Angeles)".
var trailingParenthetical = regexp.MustCompile(`^(.+?)\s*\(([^()]+)\)\s*$`)

// claudeResetLayouts are the reset-timestamp forms the CLI prints. Neither
// carries a year — see resolveClaudeYear.
var claudeResetLayouts = []string{"Jan 2 at 3:04pm", "Jan 2 at 3pm"}

// ClaudeProbe reads the Claude subscription allowance by running the CLI's own
// usage command non-interactively.
//
// There is no persisted local limit signal to read instead: the usage data,
// daemon status and session transcripts under ~/.claude carry consumption
// only — no limit, reset or remaining field. The CLI is the surface that
// states what is left.
//
// The probe itself is free. `claude -p "/usage" --output-format json` reports
// zero input tokens, zero output tokens and zero cost in its own envelope: the
// slash command is answered locally and never reaches the model, so sampling
// does not draw down the allowance it measures.
type ClaudeProbe struct {
	bin     string
	home    string
	timeout time.Duration
	// run executes the probe command. Replaced in tests so parsing can be
	// exercised without a provider.
	run func(ctx context.Context, bin string, args ...string) ([]byte, error)
	now func() time.Time
}

// NewClaudeProbe returns a probe that reads the Claude allowance via the
// `claude` CLI on PATH.
func NewClaudeProbe() *ClaudeProbe {
	home, _ := os.UserHomeDir() //nolint:errcheck // an unreadable home only costs the plan label
	return &ClaudeProbe{
		bin:     "claude",
		home:    home,
		timeout: claudeProbeTimeout,
		run:     runCommand,
		now:     time.Now,
	}
}

// Provider names the provider this probe reads.
func (p *ClaudeProbe) Provider() string { return ProviderClaude }

// Read runs the usage command and returns the windows it states.
func (p *ClaudeProbe) Read(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	args := []string{"-p", "/usage", "--output-format", "json"}
	out, err := p.run(ctx, p.bin, args...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("running %s %s: %w", p.bin, strings.Join(args, " "), err)
	}
	text, err := extractClaudeResult(out)
	if err != nil {
		return Snapshot{}, fmt.Errorf("reading %s usage output: %w", p.bin, err)
	}
	// The command answers from local state, so the reading is current as of
	// the moment it returned — unlike Codex, there is no publication lag.
	observedAt := p.now()
	windows, err := parseClaudeUsage(text, observedAt)
	if err != nil {
		return Snapshot{}, fmt.Errorf("parsing %s usage output: %w", p.bin, err)
	}
	return Snapshot{
		Plan:    p.plan(),
		Source:  fmt.Sprintf("%s %s", p.bin, strings.Join(args, " ")),
		Windows: windows,
	}, nil
}

// plan reads the account-tier labels the credentials file carries. Only the two
// tier labels are decoded; the tokens in that file are never bound to a value.
// A missing or unreadable file costs the label and nothing else.
func (p *ClaudeProbe) plan() string {
	if p.home == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(p.home, ".claude", ".credentials.json"))
	if err != nil {
		return ""
	}
	return parseClaudePlan(raw)
}

// claudeResult is the `--output-format json` envelope. Using the envelope
// rather than raw stdout keeps banner text and warnings out of the parser.
type claudeResult struct {
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// extractClaudeResult pulls the usage text out of the CLI's JSON envelope.
func extractClaudeResult(raw []byte) (string, error) {
	var env claudeResult
	if err := json.Unmarshal(bytes.TrimSpace(raw), &env); err != nil {
		return "", fmt.Errorf("decoding result envelope: %w", err)
	}
	if env.IsError {
		return "", fmt.Errorf("provider reported an error: %s", strings.TrimSpace(env.Result))
	}
	if strings.TrimSpace(env.Result) == "" {
		return "", errors.New("result envelope carried no text")
	}
	return env.Result, nil
}

// claudeCredentials decodes only the two account-tier labels. The struct is
// deliberately narrow so no credential material can flow through this package.
type claudeCredentials struct {
	OAuth struct {
		SubscriptionType string `json:"subscriptionType"`
		RateLimitTier    string `json:"rateLimitTier"`
	} `json:"claudeAiOauth"`
}

// parseClaudePlan renders the account tier as "subscription/tier", or whichever
// half is present. It returns empty rather than an error: the tier is a
// calibration label, and its absence must not fail a reading.
func parseClaudePlan(raw []byte) string {
	var creds claudeCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return ""
	}
	parts := make([]string, 0, 2)
	if s := creds.OAuth.SubscriptionType; s != "" {
		parts = append(parts, s)
	}
	if s := creds.OAuth.RateLimitTier; s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "/")
}

// parseClaudeUsage turns the usage text into provider-stated windows.
//
// Sub-cap lines (a model-scoped weekly window) state a percentage but no
// reset. They are not independent pools — they draw from the same budget as
// the unscoped window of the same name — so they inherit that window's
// boundaries and declare the inheritance.
func parseClaudeUsage(text string, observedAt time.Time) ([]Window, error) {
	var windows []Window
	for _, line := range strings.Split(text, "\n") {
		m := claudeUsageLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		used, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return nil, fmt.Errorf("reading percentage from %q: %w", strings.TrimSpace(line), err)
		}
		name, scope := splitClaudeLabel(m[1])
		w := Window{
			Name:        name,
			ModelScope:  scope,
			UsedPercent: used,
			ObservedAt:  observedAt,
		}
		if d, ok := claudeWindowDuration(name); ok {
			w.Duration = d
			w.Inferred = append(w.Inferred, "duration")
		}
		if resetText := strings.TrimSpace(m[3]); resetText != "" {
			reset, inferredZone, err := parseClaudeReset(resetText, observedAt)
			if err != nil {
				return nil, fmt.Errorf("reading reset time from %q: %w", strings.TrimSpace(line), err)
			}
			w.ResetsAt = reset
			if inferredZone {
				w.Inferred = append(w.Inferred, "timezone")
			}
		}
		windows = append(windows, w)
	}
	if len(windows) == 0 {
		return nil, errors.New("no usage windows found in output")
	}
	inheritClaudeResets(windows)
	return windows, nil
}

// inheritClaudeResets gives every sub-cap the boundaries of the unscoped window
// it draws from. A sub-cap with nothing to inherit from keeps its percentage
// but loses its duration, so it reports as unknown rather than as expired.
func inheritClaudeResets(windows []Window) {
	for i := range windows {
		if !windows[i].ResetsAt.IsZero() {
			continue
		}
		inherited := false
		for j := range windows {
			if j == i || windows[j].Name != windows[i].Name || windows[j].ResetsAt.IsZero() {
				continue
			}
			windows[i].ResetsAt = windows[j].ResetsAt
			windows[i].Duration = windows[j].Duration
			windows[i].Inferred = append(windows[i].Inferred, "resets_at")
			inherited = true
			break
		}
		if !inherited {
			windows[i].Duration = 0
		}
	}
}

// splitClaudeLabel separates a window label into its name and model scope.
// "week (all models)" is the provider's way of saying the window is unscoped.
func splitClaudeLabel(label string) (name, scope string) {
	label = strings.TrimSpace(label)
	m := trailingParenthetical.FindStringSubmatch(label)
	if m == nil {
		return label, ""
	}
	name = strings.TrimSpace(m[1])
	qualifier := strings.TrimSpace(m[2])
	if strings.EqualFold(qualifier, "all models") {
		return name, ""
	}
	return name, qualifier
}

// claudeWindowDuration maps a window name to its length. An unrecognized name
// yields no duration, which surfaces as an unknown pace rather than a guess.
func claudeWindowDuration(name string) (time.Duration, bool) {
	switch {
	case strings.EqualFold(name, "session"):
		return ClaudeSessionWindow, true
	case strings.EqualFold(name, "week"):
		return ClaudeWeeklyWindow, true
	default:
		return 0, false
	}
}

// parseClaudeReset reads "Aug 11 at 5pm (America/Los_Angeles)". It reports
// whether the zone had to be inferred because the named one could not be
// loaded (a host without tzdata), in which case the observation's own zone
// stands in.
func parseClaudeReset(text string, observedAt time.Time) (reset time.Time, inferredZone bool, err error) {
	stamp := strings.TrimSpace(text)
	loc := observedAt.Location()
	if m := trailingParenthetical.FindStringSubmatch(stamp); m != nil {
		stamp = strings.TrimSpace(m[1])
		named, loadErr := time.LoadLocation(strings.TrimSpace(m[2]))
		if loadErr != nil {
			inferredZone = true
		} else {
			loc = named
		}
	} else {
		inferredZone = true
	}

	for _, layout := range claudeResetLayouts {
		t, parseErr := time.ParseInLocation(layout, stamp, loc)
		if parseErr != nil {
			continue
		}
		return resolveClaudeYear(t, observedAt, loc), inferredZone, nil
	}
	return time.Time{}, false, fmt.Errorf("unrecognized timestamp %q", stamp)
}

// resolveClaudeYear supplies the year the CLI omits. A reset is always within a
// window's length of the observation, so the correct year is whichever of the
// neighboring three puts the reset closest to it — which is what makes a
// December reading of a January reset land in the next year.
func resolveClaudeYear(parsed, observedAt time.Time, loc *time.Location) time.Time {
	var best time.Time
	for _, year := range []int{observedAt.Year() - 1, observedAt.Year(), observedAt.Year() + 1} {
		candidate := time.Date(year, parsed.Month(), parsed.Day(),
			parsed.Hour(), parsed.Minute(), parsed.Second(), 0, loc)
		if best.IsZero() || absDuration(candidate.Sub(observedAt)) < absDuration(best.Sub(observedAt)) {
			best = candidate
		}
	}
	return best
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// runCommand executes a probe command and returns its stdout, folding stderr
// into the error so a failed probe says why.
func runCommand(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
