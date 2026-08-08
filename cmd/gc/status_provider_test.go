package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

type statusProbeProvider struct {
	runtime.Provider
	delay       atomic.Int64
	running     atomic.Bool
	liveness    atomic.Value
	observeCall atomic.Int32
}

func newStatusProbeProvider() *statusProbeProvider {
	p := &statusProbeProvider{Provider: runtime.NewFake()}
	p.liveness.Store(runtime.Liveness{})
	return p
}

func (p *statusProbeProvider) IsRunning(string) bool {
	time.Sleep(time.Duration(p.delay.Load()))
	return p.running.Load()
}

func (p *statusProbeProvider) ObserveLiveness(string, []string) runtime.Liveness {
	p.observeCall.Add(1)
	return p.liveness.Load().(runtime.Liveness)
}

func TestStatusProviderTimeoutDoesNotStickAcrossCalls(t *testing.T) {
	origTimeout := statusProviderCallTimeout
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() {
		statusProviderCallTimeout = origTimeout
		statusProviderTimeoutWarning = origWarn
	})
	statusProviderCallTimeout = 10 * time.Millisecond
	var warnings atomic.Int32
	statusProviderTimeoutWarning = func() {
		warnings.Add(1)
	}

	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(100 * time.Millisecond))
	wrapped := newBoundedStatusProvider(base)

	if wrapped.IsRunning("worker") {
		t.Fatal("first IsRunning returned true, want timeout fallback false")
	}
	base.delay.Store(0)
	if !wrapped.IsRunning("worker") {
		t.Fatal("second IsRunning returned false, want fresh provider result after timeout")
	}
	if got := warnings.Load(); got != 1 {
		t.Fatalf("timeout warnings = %d, want 1", got)
	}
}

func TestStatusProviderPreservesNativeLivenessObservation(t *testing.T) {
	base := newStatusProbeProvider()
	base.liveness.Store(runtime.Liveness{Running: true, Alive: true})
	wrapped := newBoundedStatusProvider(base)

	got := runtime.ObserveLiveness(wrapped, "worker", []string{"agent"})
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness = %#v, want running+alive from native observer", got)
	}
	if calls := base.observeCall.Load(); calls != 1 {
		t.Fatalf("ObserveLiveness calls = %d, want 1", calls)
	}
}

func TestStatusProbeTimeoutDefaultsCompose(t *testing.T) {
	// ga-u5hh regression guard: a 50ms per-call default flipped gc status to
	// partial on healthy cities. Three sequential tmux round-trips under the
	// status command's own 8-way fan-out already measure ~40-50ms on a lightly
	// loaded host, and reconciler dispatch load pushes single round-trips well
	// past 50ms. The per-call bound must keep real headroom over a loaded tmux
	// round-trip.
	if statusProviderCallTimeout < 250*time.Millisecond {
		t.Fatalf("statusProviderCallTimeout = %s, want >= 250ms headroom over loaded tmux round-trips", statusProviderCallTimeout)
	}
	// One observation issues up to statusObservationCallBudget bounded
	// provider calls sequentially; an observation bound tighter than that
	// budget re-introduces the same false partial one level up.
	if statusObservationTimeout < statusObservationCallBudget*statusProviderCallTimeout {
		t.Fatalf("statusObservationTimeout = %s, want >= %d x statusProviderCallTimeout (%s)",
			statusObservationTimeout, statusObservationCallBudget, statusProviderCallTimeout)
	}
}

func TestStatusProbeTimeoutEnvOverride(t *testing.T) {
	origCall := statusProviderCallTimeout
	origObs := statusObservationTimeout
	t.Cleanup(func() {
		statusProviderCallTimeout = origCall
		statusObservationTimeout = origObs
	})

	t.Setenv(statusProbeTimeoutEnvVar, "2s")
	applyStatusProbeTimeoutEnv()
	if statusProviderCallTimeout != 2*time.Second {
		t.Fatalf("statusProviderCallTimeout = %s, want 2s from env override", statusProviderCallTimeout)
	}
	if want := statusObservationCallBudget * 2 * time.Second; statusObservationTimeout != want {
		t.Fatalf("statusObservationTimeout = %s, want %s derived from env override", statusObservationTimeout, want)
	}
}

func TestStatusProbeTimeoutEnvOverrideKeepsObservationFloor(t *testing.T) {
	origCall := statusProviderCallTimeout
	origObs := statusObservationTimeout
	t.Cleanup(func() {
		statusProviderCallTimeout = origCall
		statusObservationTimeout = origObs
	})

	t.Setenv(statusProbeTimeoutEnvVar, "100ms")
	applyStatusProbeTimeoutEnv()
	if statusProviderCallTimeout != 100*time.Millisecond {
		t.Fatalf("statusProviderCallTimeout = %s, want 100ms from env override", statusProviderCallTimeout)
	}
	if statusObservationTimeout != defaultStatusObservationTimeout {
		t.Fatalf("statusObservationTimeout = %s, want default floor %s for a small per-call override",
			statusObservationTimeout, defaultStatusObservationTimeout)
	}
}

func TestStatusProbeTimeoutEnvZeroDisablesBounds(t *testing.T) {
	origCall := statusProviderCallTimeout
	origObs := statusObservationTimeout
	t.Cleanup(func() {
		statusProviderCallTimeout = origCall
		statusObservationTimeout = origObs
	})

	t.Setenv(statusProbeTimeoutEnvVar, "0")
	applyStatusProbeTimeoutEnv()
	if statusProviderCallTimeout != 0 {
		t.Fatalf("statusProviderCallTimeout = %s, want 0 (bounds disabled)", statusProviderCallTimeout)
	}
	if statusObservationTimeout != 0 {
		t.Fatalf("statusObservationTimeout = %s, want 0 (bounds disabled)", statusObservationTimeout)
	}
}

func TestStatusProbeTimeoutEnvUnsetLeavesSeamsAlone(t *testing.T) {
	origCall := statusProviderCallTimeout
	origObs := statusObservationTimeout
	t.Cleanup(func() {
		statusProviderCallTimeout = origCall
		statusObservationTimeout = origObs
	})

	t.Setenv(statusProbeTimeoutEnvVar, "")
	statusProviderCallTimeout = 12 * time.Millisecond
	statusObservationTimeout = 34 * time.Millisecond
	applyStatusProbeTimeoutEnv()
	if statusProviderCallTimeout != 12*time.Millisecond || statusObservationTimeout != 34*time.Millisecond {
		t.Fatalf("timeouts = (%s, %s), want test-seam values untouched when env is unset",
			statusProviderCallTimeout, statusObservationTimeout)
	}
}

func TestStatusProbeTimeoutEnvInvalidWarnsAndKeepsDefaults(t *testing.T) {
	origCall := statusProviderCallTimeout
	origObs := statusObservationTimeout
	origWarn := statusProbeTimeoutEnvWarning
	t.Cleanup(func() {
		statusProviderCallTimeout = origCall
		statusObservationTimeout = origObs
		statusProbeTimeoutEnvWarning = origWarn
	})
	var warned atomic.Int32
	statusProbeTimeoutEnvWarning = func(string) { warned.Add(1) }

	for _, raw := range []string{"bogus", "-5s"} {
		t.Setenv(statusProbeTimeoutEnvVar, raw)
		applyStatusProbeTimeoutEnv()
		if statusProviderCallTimeout != origCall || statusObservationTimeout != origObs {
			t.Fatalf("timeouts = (%s, %s) after %q, want untouched (%s, %s)",
				statusProviderCallTimeout, statusObservationTimeout, raw, origCall, origObs)
		}
	}
	if got := warned.Load(); got != 2 {
		t.Fatalf("invalid-env warnings = %d, want 2", got)
	}
}

func TestStatusProviderTimeoutMarksPartial(t *testing.T) {
	origTimeout := statusProviderCallTimeout
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() {
		statusProviderCallTimeout = origTimeout
		statusProviderTimeoutWarning = origWarn
	})
	statusProviderCallTimeout = 10 * time.Millisecond
	statusProviderTimeoutWarning = func() {}

	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(100 * time.Millisecond))
	wrapped := newBoundedStatusProvider(base)

	if wrapped.IsRunning("worker") {
		t.Fatal("IsRunning returned true, want timeout fallback false")
	}
	if !statusProviderPartial(wrapped) {
		t.Fatal("statusProviderPartial = false, want true after runtime probe timeout")
	}
}
