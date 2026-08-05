package quota

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	// codexRolloutScanLimit caps how many rollout files the probe opens. The
	// freshest reading is in one of the most recently written files; scanning
	// a handful covers concurrent sessions without walking a session tree that
	// grows without bound.
	codexRolloutScanLimit = 8

	// codexMaxLineBytes bounds a single rollout line. Rollout files interleave
	// small telemetry events with whole message payloads; a rate-limit entry is
	// around a kilobyte, so anything past this cap is skipped unread rather
	// than buffered.
	codexMaxLineBytes = 256 * 1024
)

// codexRateLimitMarker is the byte sequence a line must contain to be worth
// decoding. Most rollout lines are message payloads and would otherwise be
// parsed as JSON only to be discarded.
var codexRateLimitMarker = []byte(`"rate_limits"`)

// CodexProbe reads the Codex subscription allowance from the rollout logs the
// CLI already writes.
//
// Codex persists its own rate-limit readings, so this probe costs nothing and
// needs no invocation — but the reading is only as fresh as the last time
// Codex wrote one. Readings tens of minutes stale are normal, which is why
// every window carries the timestamp of the entry it came from.
type CodexProbe struct {
	home string
}

// NewCodexProbe returns a probe that reads Codex rollout logs under ~/.codex.
func NewCodexProbe() *CodexProbe {
	home, _ := os.UserHomeDir() //nolint:errcheck // an unreadable home surfaces as a probe error below
	return &CodexProbe{home: home}
}

// Provider names the provider this probe reads.
func (p *CodexProbe) Provider() string { return ProviderCodex }

// Read returns the freshest rate-limit reading across the most recently
// written rollout files.
//
// It deliberately does not consult ~/.codex/state_5.sqlite for the current
// thread's rollout path. Rollout mtime already identifies the file that was
// written most recently, which is the same file by a cheaper route, and it
// avoids opening a multi-megabyte database that a running Codex holds open.
func (p *CodexProbe) Read(_ context.Context) (Snapshot, error) {
	if p.home == "" {
		return Snapshot{}, fmt.Errorf("locating codex sessions: no home directory")
	}
	root := filepath.Join(p.home, ".codex", "sessions")
	paths, err := recentCodexRollouts(root, codexRolloutScanLimit)
	if err != nil {
		return Snapshot{}, err
	}
	if len(paths) == 0 {
		return Snapshot{}, fmt.Errorf("no codex rollout files under %s", root)
	}

	var best codexReading
	var bestPath string
	var warnings []string
	for _, path := range paths {
		r, ok, fileWarnings, err := readCodexRollout(path)
		if err != nil {
			return Snapshot{}, fmt.Errorf("reading %s: %w", path, err)
		}
		for _, w := range fileWarnings {
			warnings = append(warnings, fmt.Sprintf("%s: %s", path, w))
		}
		if !ok {
			continue
		}
		if bestPath == "" || r.observedAt.After(best.observedAt) {
			best, bestPath = r, path
		}
	}
	if bestPath == "" {
		return Snapshot{Warnings: warnings}, fmt.Errorf("no rate-limit readings in the %d most recent rollout files under %s", len(paths), root)
	}
	return Snapshot{Plan: best.plan, Source: bestPath, Windows: best.windows, Warnings: warnings}, nil
}

// recentCodexRollouts returns up to limit rollout paths, most recently written
// first. The session tree is laid out as YYYY/MM/DD, so the glob is fixed-depth
// and never walks arbitrary subdirectories.
func recentCodexRollouts(root string, limit int) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, "*", "*", "*", "rollout-*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("scanning %s: %w", root, err)
	}
	type entry struct {
		path string
		mod  time.Time
	}
	entries := make([]entry, 0, len(matches))
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			// A session file can be rotated away mid-scan; that costs one
			// candidate, not the reading.
			continue
		}
		entries = append(entries, entry{path: m, mod: info.ModTime()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.After(entries[j].mod) })
	if len(entries) > limit {
		entries = entries[:limit]
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.path)
	}
	return paths, nil
}

// codexReading is one rate-limit entry from a rollout log.
type codexReading struct {
	plan       string
	observedAt time.Time
	windows    []Window
}

// codexRolloutLine is the slice of a rollout entry this probe decodes.
type codexRolloutLine struct {
	Timestamp time.Time `json:"timestamp"`
	Payload   struct {
		RateLimits *struct {
			Primary   *codexLimitWindow `json:"primary"`
			Secondary *codexLimitWindow `json:"secondary"`
			PlanType  string            `json:"plan_type"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

// codexLimitWindow is one provider-stated allowance window. Codex states the
// window length and the reset instant outright, so nothing here is inferred.
type codexLimitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

// readCodexRollout returns the last rate-limit reading in a rollout file, and
// false when the file carries none.
//
// A rate-limit line that will not decode becomes a warning rather than an
// error: the freshest rollout is being appended to as this runs, so its final
// line can be half-written. Warnings are returned so a partially unreadable
// file never undercounts without a trace.
func readCodexRollout(path string) (reading codexReading, found bool, warnings []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return codexReading{}, false, nil, err
	}
	defer f.Close() //nolint:errcheck // read-only

	br := bufio.NewReaderSize(f, 64*1024)
	for lineNum := 1; ; lineNum++ {
		line, skipped, err := readBoundedLine(br, codexMaxLineBytes)
		if err != nil {
			return codexReading{}, false, warnings, err
		}
		if line == nil && !skipped {
			break
		}
		if skipped || !bytes.Contains(line, codexRateLimitMarker) {
			continue
		}
		r, ok, err := parseCodexReading(line)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped unreadable rate-limit entry on line %d: %v", lineNum, err))
			continue
		}
		if ok {
			reading, found = r, true
		}
	}
	return reading, found, warnings, nil
}

// readBoundedLine reads one line, reporting skipped when the line exceeded
// maxBytes and was discarded. It returns a nil line with skipped false at end
// of input.
func readBoundedLine(br *bufio.Reader, maxBytes int) (line []byte, skipped bool, err error) {
	var buf []byte
	over := false
	for {
		chunk, isPrefix, readErr := br.ReadLine()
		if readErr != nil {
			if len(buf) > 0 || over {
				return buf, over, nil
			}
			if errors.Is(readErr, io.EOF) {
				return nil, false, nil
			}
			return nil, false, readErr
		}
		if !over {
			if len(buf)+len(chunk) > maxBytes {
				over = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if !isPrefix {
			return buf, over, nil
		}
	}
}

// parseCodexReading decodes one rollout line, reporting false for the lines
// that carry no rate-limit entry (most of them).
func parseCodexReading(line []byte) (codexReading, bool, error) {
	if !bytes.Contains(line, codexRateLimitMarker) {
		return codexReading{}, false, nil
	}
	var entry codexRolloutLine
	if err := json.Unmarshal(line, &entry); err != nil {
		return codexReading{}, false, fmt.Errorf("decoding rollout entry: %w", err)
	}
	limits := entry.Payload.RateLimits
	if limits == nil {
		return codexReading{}, false, nil
	}

	r := codexReading{plan: limits.PlanType, observedAt: entry.Timestamp}
	for _, slot := range []*codexLimitWindow{limits.Primary, limits.Secondary} {
		if slot == nil {
			continue
		}
		r.windows = append(r.windows, Window{
			Name:        codexWindowName(slot.WindowMinutes),
			UsedPercent: slot.UsedPercent,
			Duration:    time.Duration(slot.WindowMinutes) * time.Minute,
			ResetsAt:    time.Unix(slot.ResetsAt, 0),
			ObservedAt:  entry.Timestamp,
		})
	}
	if len(r.windows) == 0 {
		return codexReading{}, false, nil
	}
	return r, true, nil
}

// codexWindowName labels a window by its provider-stated length, so a Codex
// week and a Claude week print under the same name and compare at a glance.
func codexWindowName(minutes int) string {
	switch minutes {
	case 10080:
		return "week"
	case 1440:
		return "day"
	case 300:
		return "session"
	case 0:
		return "window"
	}
	if minutes%60 == 0 {
		return fmt.Sprintf("%dh", minutes/60)
	}
	return fmt.Sprintf("%dm", minutes)
}
