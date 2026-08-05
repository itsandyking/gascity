package quota

import (
	"strings"
	"time"
)

// RatioState classifies why a [Pace.Ratio] may not be a plain comparable
// number, so a caller acting on the meter can name the reason rather than
// guessing at a zero.
type RatioState string

const (
	// RatioOK means Ratio is finite and comparable.
	RatioOK RatioState = "ok"
	// RatioColdStart means no measurable time has elapsed in the window yet,
	// so there is no burn rate to compare. Immediately after a reset there is
	// no prior sample; the window is unpaced until time accrues.
	RatioColdStart RatioState = "cold-start"
	// RatioExhausted means the window has no allowance left. It binds
	// absolutely: no rate keeps a request inside a window already at 100%.
	RatioExhausted RatioState = "exhausted"
	// RatioExpired means the observed window's reset has already passed, so
	// the reading describes a window that no longer exists. The percentage has
	// jumped back up since; this is a reset, not negative burn.
	RatioExpired RatioState = "expired"
	// RatioUnknown means the window lacks a length, so elapsed time — and
	// therefore every rate derived from it — is unknowable.
	RatioUnknown RatioState = "unknown"
)

// Pace is one [Window] with the pacing arithmetic derived for a single
// instant. Every rate is expressed in percent of that window per hour, which
// keeps the figures comparable within a provider; [Pace.Ratio] is the
// dimensionless form that compares across providers.
type Pace struct {
	Window

	// Age is how stale the reading is at evaluation time. Mandatory context,
	// not decoration: a ratio presented without its age invites acting on a
	// number the provider wrote an hour ago.
	Age time.Duration

	// Elapsed is how much of the window has passed, clamped to [0, Duration].
	Elapsed time.Duration
	// Remaining is the time left until reset, clamped at zero.
	Remaining time.Duration
	// ElapsedPercent is Elapsed as a percentage of Duration, 0-100.
	ElapsedPercent float64

	// BurnPercentPerHour is the average rate the window has actually been
	// consumed at so far.
	BurnPercentPerHour float64
	// AllowedPercentPerHour is the rate that spends exactly the remaining
	// allowance over exactly the remaining time — the pace that finishes the
	// window at 100%.
	AllowedPercentPerHour float64
	// Ratio is BurnPercentPerHour / AllowedPercentPerHour, zero unless State
	// is [RatioOK].
	Ratio float64
	// State says whether Ratio is usable, and why not when it is not.
	State RatioState

	// ExhaustsAt is when UsedPercent reaches 100 if the current burn rate
	// holds. Zero when the window never exhausts at this rate (including zero
	// burn) or when no rate could be derived.
	ExhaustsAt time.Time

	// Confidence is the fraction of the window elapsed, 0-1. A projection from
	// 3% of a window is arithmetic, not a forecast; weight it accordingly.
	Confidence float64
}

// PaceAt derives the pacing arithmetic for this window at now.
//
// The provider states used percentage, window length and reset time. Elapsed
// time falls out of the last two, and both rates fall out of elapsed:
//
//	burn    = used / elapsed
//	allowed = (100 - used) / time_left
//	ratio   = burn / allowed
//
// Nothing is cached between calls. The provider can re-throttle mid-window, so
// every figure is re-derived from the reading in hand.
func (w Window) PaceAt(now time.Time) Pace {
	p := Pace{Window: w, State: RatioOK}
	if !w.ObservedAt.IsZero() {
		// Clamped at zero: a reading cannot be younger than nothing. It can
		// look that way either because the evaluation instant was captured
		// before the probes ran, or because the provider stamped the reading
		// with its own clock.
		if age := now.Sub(w.ObservedAt); age > 0 {
			p.Age = age
		}
	}

	if w.Duration <= 0 {
		p.State = RatioUnknown
		return p
	}

	remaining := w.ResetsAt.Sub(now)
	if remaining <= 0 {
		p.State = RatioExpired
		return p
	}
	if remaining > w.Duration {
		// The reset is further out than the window is long — clock skew or a
		// provider re-anchor. Treat it as the start of the window rather than
		// letting elapsed go negative and invert every rate.
		remaining = w.Duration
	}
	p.Remaining = remaining

	p.Elapsed = w.Duration - remaining
	p.ElapsedPercent = float64(p.Elapsed) / float64(w.Duration) * 100
	p.Confidence = float64(p.Elapsed) / float64(w.Duration)

	if p.Elapsed <= 0 {
		p.State = RatioColdStart
		return p
	}
	p.BurnPercentPerHour = w.UsedPercent / p.Elapsed.Hours()

	remainingPercent := 100 - w.UsedPercent
	if remainingPercent <= 0 {
		p.State = RatioExhausted
		p.ExhaustsAt = now
		return p
	}
	p.AllowedPercentPerHour = remainingPercent / remaining.Hours()
	p.Ratio = p.BurnPercentPerHour / p.AllowedPercentPerHour

	if p.BurnPercentPerHour > 0 {
		toExhaustion := time.Duration(remainingPercent / p.BurnPercentPerHour * float64(time.Hour))
		p.ExhaustsAt = now.Add(toExhaustion)
	}
	return p
}

// DeadTime is how long the window sits exhausted before it resets: the failure
// mode of spending an allowance early and then idling. It is zero when the
// window is projected to last to its reset, which is the healthy case.
func (p Pace) DeadTime() time.Duration {
	if p.ExhaustsAt.IsZero() || p.ExhaustsAt.After(p.ResetsAt) {
		return 0
	}
	return p.ResetsAt.Sub(p.ExhaustsAt)
}

// bindingRank orders states by how absolutely they constrain a request, so a
// comparison never lets a stale window outrank a live one. Ratios are only
// compared between windows of equal rank.
func bindingRank(s RatioState) int {
	switch s {
	case RatioExhausted:
		return 4
	case RatioOK:
		return 3
	case RatioColdStart:
		return 2
	case RatioUnknown:
		return 1
	default: // RatioExpired
		return 0
	}
}

// Binding returns the window that constrains a request to the given model
// first, and false when no window gates it.
//
// Windows gate conjunctively: a request is servable only if every window that
// gates it has headroom, so the binding constraint is the hottest of them.
// Choosing a different model within a provider cannot dodge that provider's
// shared window — there is no slacker window to route toward inside a
// provider. Rebalancing toward slack is a cross-provider move.
//
// The empty model asks for the floor every request to this provider faces,
// which is the hottest unscoped window.
func (r Report) Binding(model string) (Pace, bool) {
	var best Pace
	found := false
	for _, p := range r.Windows {
		if !p.Gates(model) {
			continue
		}
		if !found {
			best, found = p, true
			continue
		}
		switch {
		case bindingRank(p.State) > bindingRank(best.State):
			best = p
		case bindingRank(p.State) == bindingRank(best.State) && p.Ratio > best.Ratio:
			best = p
		}
	}
	return best, found
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
