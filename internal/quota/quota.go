// Package quota is the percentage-remaining meter: it reads what each provider
// says about how much of gascity's own allowance is left, and derives the rate
// the city may spend it at to finish the window at ~100%.
//
// It answers "how much is left, and how fast may we spend it" — the half that
// routing and pacing decisions need. Consumption accounting ("what did we
// spend") belongs to internal/usage; this package never reconstructs an
// allowance from local counters.
//
// # Why the provider is the only source
//
// Subscription plans are not denominated in tokens. You do not buy N tokens a
// week; you get a multiplier over a baseline burn rate that the provider can
// throttle up or down mid-window. There is therefore no ceiling to declare in
// config — a declared number is not merely stale, it is the wrong kind of
// quantity. Provider-reported percentage within a window is the ground truth,
// and every figure here is either read from the provider or derived from two
// such readings.
//
// # The ratio is the control variable
//
//	ratio = burn_rate / allowed_rate
//	      = (pct_used / time_elapsed) / (pct_remaining / time_left)
//
// It is dimensionless, so it compares directly across providers with different
// windows, units and plan types:
//
//	ratio < 1  under pace, headroom to spend
//	ratio ~ 1  on pace to finish the window at ~100%
//	ratio > 1  will exhaust before reset
//
// Under-use is a failure mode too: unused allowance in a window is forfeited,
// not banked. The goal is finishing at ~100%, not staying under.
//
// # Structure
//
// The package splits at the I/O boundary. [Probe] implementations touch the
// outside world (a file read, an exec) and return raw provider-stated
// [Window]s; [Window.PaceAt] and [Report.Binding] are pure arithmetic over
// those readings and are testable without a provider.
//
// This package is a read. It never writes provider state, never decides what
// to do about a hot ratio, and holds no pacing policy — the consumer of the
// number is a judging agent, not a control loop.
package quota

import (
	"context"
	"sort"
	"time"
)

// Provider names, used as the [Report.Provider] key and as the selector for
// gc quota's per-provider output. Metered providers are the ones with an
// allowance; local providers bind on throughput instead and are not read here.
const (
	// ProviderClaude is the Anthropic Claude Code subscription.
	ProviderClaude = "claude"
	// ProviderCodex is the OpenAI Codex CLI subscription.
	ProviderCodex = "codex"
)

// Window is one provider-reported allowance window as observed at an instant.
// It carries only what the provider stated (plus what gascity had to infer to
// make the statement usable), never a derived rate — see [Pace] for those.
type Window struct {
	// Name identifies the window within its provider, e.g. "session" or
	// "week". Names are provider vocabulary, not a gascity taxonomy.
	Name string

	// ModelScope names the model family this window gates, and is empty when
	// the window gates every request to the provider. Claude's
	// "Current week (Fable)" line becomes ModelScope "Fable"; its
	// "Current week (all models)" line has no scope.
	//
	// A scoped window is a sub-cap inside the provider's shared budget, not an
	// independent pool: spending it draws the unscoped windows down too. A
	// sub-cap reading 0% is therefore a possible blocker, never free capacity.
	ModelScope string

	// UsedPercent is the provider-reported consumption of this window, 0-100.
	UsedPercent float64

	// Duration is the window length. Zero means neither the provider stated it
	// nor could gascity infer it, which makes the window unusable for pacing.
	Duration time.Duration

	// ResetsAt is when this window's allowance returns to 0%.
	ResetsAt time.Time

	// ObservedAt is when the reading was taken. Provider signals are written
	// asynchronously and can be materially stale, so every derived figure
	// carries this reading's age with it.
	ObservedAt time.Time

	// Inferred lists the attributes gascity derived rather than read from the
	// provider ("duration", "resets_at", "timezone"), so a decision made from
	// this window can name exactly what was assumed.
	Inferred []string
}

// Gates reports whether this window constrains a request to the given model.
// An unscoped window gates every request; a scoped window gates only models
// whose id contains its scope. The empty model means "any request", which no
// scoped window gates on its own.
func (w Window) Gates(model string) bool {
	if w.ModelScope == "" {
		return true
	}
	return containsFold(model, w.ModelScope)
}

// Snapshot is one probe's raw reading of a provider.
type Snapshot struct {
	// Plan is the provider's own label for the account tier
	// ("max"/"default_claude_max_20x", "prolite"). Calibration key only — no
	// gascity behavior branches on it.
	Plan string
	// Source names where the reading came from, in terms a human can go and
	// check, e.g. a file path or the command that was run.
	Source string
	// Windows are the provider-stated allowance windows.
	Windows []Window
	// Warnings record what the probe had to skip to produce this reading, so a
	// partially unreadable source never passes as a clean one.
	Warnings []string
}

// Probe reads one provider's own report of its remaining allowance. Probes
// touch the outside world; everything derived from their output is pure.
type Probe interface {
	// Provider names the provider this probe reads.
	Provider() string
	// Read returns the provider's currently reported windows. A probe that
	// cannot read the signal returns an error rather than an empty snapshot,
	// so a missing meter is never mistaken for an idle one.
	Read(ctx context.Context) (Snapshot, error)
}

// Report is the meter for one provider, evaluated at a single instant.
type Report struct {
	// Provider is the provider key, e.g. [ProviderClaude].
	Provider string
	// Plan is the provider's account-tier label, if it states one.
	Plan string
	// Source names where the reading came from.
	Source string
	// Windows are the provider's windows with their derived pace.
	Windows []Pace
	// Warnings record what the probe had to skip to produce this reading.
	Warnings []string
	// Err is the probe failure, if the provider could not be read. A report
	// with a non-nil Err carries no windows and must not be read as "idle".
	Err error
}

// Reading is the meter across every probed provider at one instant.
type Reading struct {
	// TakenAt is the instant every [Pace] in this reading was evaluated at.
	TakenAt time.Time
	// Providers holds one report per probe, ordered by provider name so
	// repeated readings print stably.
	Providers []Report
}

// Read probes every provider and evaluates each reading at now. Probe failures
// become [Report.Err] rather than aborting the read: one unreadable provider
// must not blind the meter to the others.
func Read(ctx context.Context, now time.Time, probes ...Probe) Reading {
	reading := Reading{TakenAt: now, Providers: make([]Report, 0, len(probes))}
	for _, p := range probes {
		r := Report{Provider: p.Provider()}
		snap, err := p.Read(ctx)
		r.Warnings = snap.Warnings
		if err != nil {
			r.Err = err
		} else {
			r.Plan = snap.Plan
			r.Source = snap.Source
			r.Windows = make([]Pace, 0, len(snap.Windows))
			for _, w := range snap.Windows {
				r.Windows = append(r.Windows, w.PaceAt(now))
			}
		}
		reading.Providers = append(reading.Providers, r)
	}
	sort.Slice(reading.Providers, func(i, j int) bool {
		return reading.Providers[i].Provider < reading.Providers[j].Provider
	})
	return reading
}

// DefaultProbes returns the probes for every metered provider, in a fixed
// order. Local providers (llama-swap, Apple FM) are absent by design: they are
// free, so they never bind on allowance — they bind on throughput, which is a
// different measurement and not this meter's job.
func DefaultProbes() []Probe {
	return []Probe{NewClaudeProbe(), NewCodexProbe()}
}
