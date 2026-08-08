package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Status probe bounds. gc status and gc rig status wrap their runtime
// provider in statusProvider so a wedged runtime degrades the command to a
// clearly-labeled partial snapshot instead of hanging it. Two bounds compose:
//
//   - statusProviderCallTimeout bounds each individual provider call
//     (IsRunning, ObserveLiveness, GetMeta, ...).
//   - statusObservationTimeout bounds one full per-session observation
//     (observeSessionTargetWithWarning), which issues up to
//     statusObservationCallBudget bounded calls sequentially.
//
// The defaults must leave generous headroom over real tmux round-trips under
// load: with the status fan-out's own 8-way concurrency plus an actively
// dispatching reconciler, single round-trips routinely spike past 100ms
// (ga-u5hh: a 50ms per-call default flipped healthy cities to partial). The
// whole probe pass stays bounded without a pass-level deadline because
// observations run in parallel batches and a wedged runtime trips the
// per-call bound at most once per observation — the liveness fallback
// reports not-running, which short-circuits the rest of that observation's
// call chain.
const (
	defaultStatusProviderCallTimeout = 500 * time.Millisecond
	defaultStatusObservationTimeout  = 3 * time.Second
	// statusObservationCallBudget is the worst-case number of sequential
	// bounded provider calls one runtime-only observation issues: one provider
	// metadata lookup while resolving the handle, two session metadata lookups,
	// liveness, attachment, and last-activity.
	statusObservationCallBudget = 6
	// statusProbeTimeoutEnvVar overrides the per-call bound as a Go duration
	// ("750ms", "2s"); the observation bound scales with it. "0" disables
	// both bounds: wait indefinitely, never degrade to partial.
	statusProbeTimeoutEnvVar = "GC_STATUS_PROBE_TIMEOUT"
)

var (
	statusProviderCallTimeout    = defaultStatusProviderCallTimeout
	statusObservationTimeout     = defaultStatusObservationTimeout
	statusProviderTimeoutWarning = func() {
		fmt.Fprintln(os.Stderr, "gc status: runtime status probe timed out; using partial status")
	}
	statusProbeTimeoutEnvWarning = func(raw string) {
		fmt.Fprintf(os.Stderr, "gc status: ignoring invalid %s %q (want a Go duration like \"750ms\")\n", statusProbeTimeoutEnvVar, raw) //nolint:errcheck // best-effort stderr
	}
)

// applyStatusProbeTimeoutEnv applies the GC_STATUS_PROBE_TIMEOUT override to
// the status probe bounds. It runs at status-provider construction — the one
// seam both status commands pass through — and touches the package seams only
// when the variable is set, so tests that pin the bounds directly stay valid.
func applyStatusProbeTimeoutEnv() {
	raw := strings.TrimSpace(os.Getenv(statusProbeTimeoutEnvVar))
	if raw == "" {
		return
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		statusProbeTimeoutEnvWarning(raw)
		return
	}
	statusProviderCallTimeout = d
	statusObservationTimeout = statusObservationTimeoutFor(d)
}

// statusObservationTimeoutFor derives the per-observation bound from a
// per-call bound: the observation call budget times the per-call allowance,
// floored at the default so small overrides don't starve handle resolution.
// A non-positive per-call bound disables the observation bound too.
func statusObservationTimeoutFor(callTimeout time.Duration) time.Duration {
	if callTimeout <= 0 {
		return 0
	}
	derived := statusObservationCallBudget * callTimeout
	if derived < defaultStatusObservationTimeout {
		return defaultStatusObservationTimeout
	}
	return derived
}

type statusProvider struct {
	base     runtime.Provider
	warnOnce sync.Once
	partial  atomic.Bool
}

var _ runtime.RelaunchProvider = (*statusProvider)(nil)

func statusProviderPartial(sp any) bool {
	p, ok := sp.(*statusProvider)
	return ok && p.partial.Load()
}

func markStatusProviderPartial(sp any) {
	if p, ok := sp.(*statusProvider); ok {
		p.partial.Store(true)
	}
}

func (p *statusProvider) StatusPartial() bool {
	return p.partial.Load()
}

func newBoundedStatusProvider(base runtime.Provider) runtime.Provider {
	if sp, ok := base.(*statusProvider); ok {
		return sp
	}
	return &statusProvider{base: base}
}

func boundedStatusCall[T any](p *statusProvider, fallback T, fn func() T) T {
	if statusProviderCallTimeout <= 0 {
		return fn()
	}
	resultCh := make(chan T, 1)
	go func() {
		resultCh <- fn()
	}()
	select {
	case result := <-resultCh:
		return result
	case <-time.After(statusProviderCallTimeout):
		p.partial.Store(true)
		p.warnOnce.Do(statusProviderTimeoutWarning)
		return fallback
	}
}

func (p *statusProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	return p.base.Start(ctx, name, cfg)
}

func (p *statusProvider) Stop(name string) error {
	return p.base.Stop(name)
}

func (p *statusProvider) Interrupt(name string) error {
	return p.base.Interrupt(name)
}

func (p *statusProvider) IsRunning(name string) bool {
	return boundedStatusCall(p, false, func() bool {
		return p.base.IsRunning(name)
	})
}

func (p *statusProvider) IsAttached(name string) bool {
	return boundedStatusCall(p, false, func() bool {
		return p.base.IsAttached(name)
	})
}

func (p *statusProvider) Attach(name string) error {
	return p.base.Attach(name)
}

func (p *statusProvider) ProcessAlive(name string, processNames []string) bool {
	return boundedStatusCall(p, false, func() bool {
		return p.base.ProcessAlive(name, processNames)
	})
}

func (p *statusProvider) ObserveLiveness(name string, processNames []string) runtime.Liveness {
	return boundedStatusCall(p, runtime.Liveness{}, func() runtime.Liveness {
		return runtime.ObserveLiveness(p.base, name, processNames)
	})
}

func (p *statusProvider) Nudge(name string, content []runtime.ContentBlock) error {
	return p.base.Nudge(name, content)
}

func (p *statusProvider) SetMeta(name, key, value string) error {
	return p.base.SetMeta(name, key, value)
}

func (p *statusProvider) GetMeta(name, key string) (string, error) {
	result := boundedStatusCall(p, struct {
		value string
		err   error
	}{}, func() struct {
		value string
		err   error
	} {
		value, err := p.base.GetMeta(name, key)
		return struct {
			value string
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) RemoveMeta(name, key string) error {
	return p.base.RemoveMeta(name, key)
}

func (p *statusProvider) Peek(name string, lines int) (string, error) {
	result := boundedStatusCall(p, struct {
		value string
		err   error
	}{}, func() struct {
		value string
		err   error
	} {
		value, err := p.base.Peek(name, lines)
		return struct {
			value string
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) ListRunning(prefix string) ([]string, error) {
	result := boundedStatusCall(p, struct {
		value []string
		err   error
	}{}, func() struct {
		value []string
		err   error
	} {
		value, err := p.base.ListRunning(prefix)
		return struct {
			value []string
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) RouteACP(name string) {
	if router, ok := p.base.(interface{ RouteACP(string) }); ok {
		router.RouteACP(name)
	}
}

func (p *statusProvider) GetLastActivity(name string) (time.Time, error) {
	result := boundedStatusCall(p, struct {
		value time.Time
		err   error
	}{}, func() struct {
		value time.Time
		err   error
	} {
		value, err := p.base.GetLastActivity(name)
		return struct {
			value time.Time
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) ClearScrollback(name string) error {
	return p.base.ClearScrollback(name)
}

func (p *statusProvider) CopyTo(name, src, relDst string) error {
	return p.base.CopyTo(name, src, relDst)
}

func (p *statusProvider) SendKeys(name string, keys ...string) error {
	return p.base.SendKeys(name, keys...)
}

func (p *statusProvider) RunLive(name string, cfg runtime.Config) error {
	return p.base.RunLive(name, cfg)
}

// Relaunch forwards a warm-box agent relaunch to the wrapped provider when it
// supports one, so the reconciler's RelaunchProvider type-assert is not masked
// by the status wrapper. Not bounded — it is a mutation, not a status probe.
func (p *statusProvider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	if rp, ok := p.base.(runtime.RelaunchProvider); ok {
		return rp.Relaunch(ctx, name, cfg)
	}
	return runtime.ErrRelaunchUnsupported
}

func (p *statusProvider) Capabilities() runtime.ProviderCapabilities {
	return p.base.Capabilities()
}

func (p *statusProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	ip, ok := p.base.(runtime.InteractionProvider)
	if !ok {
		return nil, nil
	}
	result := boundedStatusCall(p, struct {
		value *runtime.PendingInteraction
		err   error
	}{}, func() struct {
		value *runtime.PendingInteraction
		err   error
	} {
		value, err := ip.Pending(name)
		return struct {
			value *runtime.PendingInteraction
			err   error
		}{value: value, err: err}
	})
	return result.value, result.err
}

func (p *statusProvider) Respond(name string, response runtime.InteractionResponse) error {
	ip, ok := p.base.(runtime.InteractionProvider)
	if !ok {
		return runtime.ErrInteractionUnsupported
	}
	return ip.Respond(name, response)
}
