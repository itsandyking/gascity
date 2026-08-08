package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// alwaysReachable / neverReachable are injected commit-reachability oracles so
// the work-record validation is testable without a real git repo.
func alwaysReachable(string, string) bool { return true }
func neverReachable(string, string) bool  { return false }

func TestValidateWorkRecordOnClose(t *testing.T) {
	tests := []struct {
		name      string
		meta      map[string]string
		reachable func(string, string) bool
		wantViol  string // substring expected in the (single) violation; "" ⇒ no violations
	}{
		{
			name:     "no-op close passes",
			meta:     map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp},
			wantViol: "",
		},
		{
			name:     "blocked close passes",
			meta:     map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked},
			wantViol: "",
		},
		{
			name: "shipped with reachable commit passes",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "abc123",
				beadmeta.WorkBranchMetadataKey:  "bd-x",
			},
			reachable: alwaysReachable,
			wantViol:  "",
		},
		{
			name: "shipped with commit NOT reachable on branch is rejected",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "abc123",
				beadmeta.WorkBranchMetadataKey:  "bd-x",
			},
			reachable: neverReachable,
			wantViol:  "not reachable",
		},
		{
			name: "shipped without commit is rejected",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkBranchMetadataKey:  "bd-x",
			},
			reachable: alwaysReachable,
			wantViol:  beadmeta.WorkCommitMetadataKey,
		},
		{
			name: "shipped without branch is rejected",
			meta: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  "abc123",
			},
			reachable: alwaysReachable,
			wantViol:  beadmeta.WorkBranchMetadataKey,
		},
		{
			name:     "missing outcome is rejected",
			meta:     map[string]string{},
			wantViol: "missing " + beadmeta.WorkOutcomeMetadataKey,
		},
		{
			name:     "unknown outcome is rejected",
			meta:     map[string]string{beadmeta.WorkOutcomeMetadataKey: "done"},
			wantViol: "invalid",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reachable := tc.reachable
			if reachable == nil {
				reachable = neverReachable
			}
			bead := beads.Bead{ID: "wr-1", Type: "task", Metadata: tc.meta}
			got := validateWorkRecordOnClose(bead, reachable)
			if tc.wantViol == "" {
				if len(got) != 0 {
					t.Fatalf("expected no violations, got %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("expected a violation containing %q, got none", tc.wantViol)
			}
			joined := strings.Join(got, " | ")
			if !strings.Contains(joined, tc.wantViol) {
				t.Fatalf("violation %q does not contain %q", joined, tc.wantViol)
			}
		})
	}
}

func TestIsWorkRecordGatedBead(t *testing.T) {
	corroborated := func(beads.Bead) bool { return true }
	uncorroborated := func(beads.Bead) bool { return false }
	tests := []struct {
		name        string
		bead        beads.Bead
		corroborate teardownCorroborator
		want        bool
	}{
		{name: "plain task bead is gated", bead: beads.Bead{Type: "task"}, want: true},
		{name: "empty type defaults to gated", bead: beads.Bead{}, want: true},
		{
			name: "graph workflow root with recipe identity is not gated",
			bead: beads.Bead{
				Type: "task",
				Ref:  "mol-demo",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
					beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				},
			},
			want: false,
		},
		{
			name: "controller-routed run marker is not gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey:     beadmeta.KindRun,
				beadmeta.RoutedToMetadataKey: "gascity/control-dispatcher",
			}},
			want: false,
		},
		{
			name: "bare controller kind is gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindCheck}},
			want: true,
		},
		{
			name: "structured scope latch is not gated",
			bead: beads.Bead{
				Type: "task",
				Ref:  "step.iteration.1",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:       beadmeta.KindScope,
					beadmeta.RootBeadIDMetadataKey: "wf-root",
					beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleBody,
					beadmeta.ControlForMetadataKey: "step",
					beadmeta.StepRefMetadataKey:    "step.iteration.1",
				},
			},
			want: false,
		},
		{name: "convoy bead is not gated", bead: beads.Bead{Type: "convoy"}, want: false},
		{name: "message bead is not gated", bead: beads.Bead{Type: "message"}, want: false},
		{name: "molecule rollup container is not gated", bead: beads.Bead{Type: "molecule"}, want: false},
		{name: "epic rollup is not gated", bead: beads.Bead{Type: "epic"}, want: false},
		{
			name: "control-dispatcher lane route keeps a control kind exempt",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey:     beadmeta.KindFanout,
				beadmeta.RoutedToMetadataKey: "gascity/control-dispatcher",
			}},
			want: false,
		},
		{
			name: "legacy workflow-control route keeps a control kind exempt",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey:     beadmeta.KindScopeCheck,
				beadmeta.RoutedToMetadataKey: "gascity/workflow-control",
			}},
			want: false,
		},
		{
			name: "gc.kind=task attempt work is gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindTask}},
			want: true,
		},
		{
			name: "arbitrary gc.kind is gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: "totally-real-bookkeeping"}},
			want: true,
		},
		{
			name: "control kind on a session-claimed bead is gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey:      beadmeta.KindCheck,
				beadmeta.SessionIDMetadataKey: "mc-1234",
			}},
			want: true,
		},
		{
			name: "control kind on a directly assigned bead is gated",
			bead: beads.Bead{
				Type:     "task",
				Assignee: "gascity/gc.implementation-worker",
				Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindCheck},
			},
			want: true,
		},
		{
			name: "control route with a stale worker assignment is gated",
			bead: beads.Bead{
				Type:     "task",
				Assignee: "gascity/gc.implementation-worker",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:     beadmeta.KindCheck,
					beadmeta.RoutedToMetadataKey: "gascity/control-dispatcher",
				},
			},
			want: true,
		},
		{
			name: "control kind with worker provenance is gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey:        beadmeta.KindDrain,
				beadmeta.WorkerGenusMetadataKey: "anthropic",
			}},
			want: true,
		},
		{
			name: "control kind routed to a worker template is gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey:     beadmeta.KindWorkflowFinalize,
				beadmeta.RoutedToMetadataKey: "gascity/gc.implementation-worker",
			}},
			want: true,
		},
		{
			name: "workflow kind on a session-claimed bead is gated",
			bead: beads.Bead{Type: "task", Ref: "mol-demo", Metadata: map[string]string{
				beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
				beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				beadmeta.SessionNameMetadataKey:     "gc__implementation-worker-mc-1",
			}},
			want: true,
		},
		{
			name: "corroborated cleanup teardown is not gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey:      beadmeta.KindCleanup,
				beadmeta.SessionIDMetadataKey: "mc-1234",
			}},
			corroborate: corroborated,
			want:        false,
		},
		{
			name: "corroborated scope-role teardown attempt is not gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey:      beadmeta.KindTask,
				beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
			}},
			corroborate: corroborated,
			want:        false,
		},
		{
			name: "uncorroborated cleanup claim is gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey:      beadmeta.KindCleanup,
				beadmeta.SessionIDMetadataKey: "mc-1234",
			}},
			corroborate: uncorroborated,
			want:        true,
		},
		{
			name: "cleanup claim with no corroborator is gated",
			bead: beads.Bead{Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindCleanup}},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWorkRecordGatedBead(tc.bead, tc.corroborate); got != tc.want {
				t.Fatalf("isWorkRecordGatedBead = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsControllerOwnedBookkeepingKind pins the candidate kind vocabulary.
// Membership alone is not the exemption; shape is tested above.
func TestIsControllerOwnedBookkeepingKind(t *testing.T) {
	var owned []string
	owned = append(owned, beadmeta.ControlKinds...)
	owned = append(owned,
		beadmeta.KindWorkflow, beadmeta.KindScope, beadmeta.KindSpec,
		beadmeta.KindRun, beadmeta.KindRetryRun, beadmeta.KindWisp, beadmeta.KindClosed)
	for _, kind := range owned {
		if !isControllerOwnedBookkeepingKind(kind) {
			t.Errorf("isControllerOwnedBookkeepingKind(%q) = false, want true", kind)
		}
	}
	for _, kind := range []string{"", beadmeta.KindTask, beadmeta.KindCleanup, "banana", "Check", "cleanup-x"} {
		if isControllerOwnedBookkeepingKind(kind) {
			t.Errorf("isControllerOwnedBookkeepingKind(%q) = true, want false", kind)
		}
	}
}

// TestIsCorroboratedTeardownBead covers the store-backed terminal-teardown
// check both ways: the genuine drain shape stays exempt, and a mid-molecule
// work step that stamps itself gc.kind=cleanup fails because work remains open.
func TestIsCorroboratedTeardownBead(t *testing.T) {
	const rootID = "wf-root"
	root := beads.Bead{ID: rootID, Type: "task", Ref: "mol-demo", Status: "in_progress", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.RootBeadIDMetadataKey:      rootID,
	}}
	closedWork := beads.Bead{ID: "wf-do-work", Type: "task", Status: "closed", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: rootID,
	}}
	openWork := beads.Bead{ID: "wf-do-work-open", Type: "task", Status: "in_progress", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: rootID,
	}}
	finalize := beads.Bead{ID: "wf-finalize", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindWorkflowFinalize,
		beadmeta.RootBeadIDMetadataKey: rootID,
		beadmeta.RoutedToMetadataKey:   "gascity/control-dispatcher",
	}}
	drain := beads.Bead{ID: "wf-drain", Type: "task", Status: "in_progress", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindCleanup,
		beadmeta.RootBeadIDMetadataKey: rootID,
		beadmeta.SessionIDMetadataKey:  "mc-1",
	}}
	openDrain := beads.Bead{ID: "wf-drain-open", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindCleanup,
		beadmeta.RootBeadIDMetadataKey: rootID,
	}}

	t.Run("terminal drain under a live workflow root is corroborated", func(t *testing.T) {
		store := beads.NewMemStoreFrom(1, []beads.Bead{root, closedWork, finalize, drain}, nil)
		if !isCorroboratedTeardownBead(store, drain) {
			t.Fatal("expected the terminal drain step to be corroborated")
		}
	})
	t.Run("teardown attempt bead with scope role is corroborated", func(t *testing.T) {
		attempt := beads.Bead{ID: "wf-teardown-attempt", Type: "task", Ref: "step.teardown", Status: "in_progress", Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindTask,
			beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleTeardown,
			beadmeta.RootBeadIDMetadataKey: rootID,
		}}
		retryControl := beads.Bead{ID: "wf-retry", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindRetry,
			beadmeta.RootBeadIDMetadataKey: rootID,
			beadmeta.RoutedToMetadataKey:   "gascity/control-dispatcher",
		}}
		scopeLatch := beads.Bead{ID: "wf-scope", Type: "task", Ref: "step", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindScope,
			beadmeta.RootBeadIDMetadataKey: rootID,
			beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleBody,
			beadmeta.ControlForMetadataKey: "step",
			beadmeta.StepRefMetadataKey:    "step",
		}}
		store := beads.NewMemStoreFrom(1, []beads.Bead{root, closedWork, finalize, retryControl, scopeLatch, attempt}, nil)
		if !isCorroboratedTeardownBead(store, attempt) {
			t.Fatal("expected the teardown attempt bead to be corroborated")
		}
	})
	t.Run("open work sibling defeats corroboration", func(t *testing.T) {
		spoofed := beads.Bead{ID: "wf-spoofed", Type: "task", Status: "in_progress", Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindCleanup,
			beadmeta.RootBeadIDMetadataKey: rootID,
		}}
		store := beads.NewMemStoreFrom(1, []beads.Bead{root, spoofed, openDrain}, nil)
		if isCorroboratedTeardownBead(store, spoofed) {
			t.Fatal("a cleanup claim with the real teardown still open must not be corroborated")
		}
	})
	t.Run("open unknown-kind sibling defeats corroboration", func(t *testing.T) {
		store := beads.NewMemStoreFrom(1, []beads.Bead{root, openWork, drain}, nil)
		if isCorroboratedTeardownBead(store, drain) {
			t.Fatal("open work under the root must defeat corroboration")
		}
	})
	t.Run("missing root linkage fails", func(t *testing.T) {
		orphan := beads.Bead{ID: "wf-orphan", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindCleanup,
		}}
		store := beads.NewMemStoreFrom(1, []beads.Bead{root, orphan}, nil)
		if isCorroboratedTeardownBead(store, orphan) {
			t.Fatal("a teardown claim without root linkage must not be corroborated")
		}
	})
	t.Run("self-referential root linkage fails", func(t *testing.T) {
		selfRoot := beads.Bead{ID: "wf-self", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindCleanup,
			beadmeta.RootBeadIDMetadataKey: "wf-self",
		}}
		store := beads.NewMemStoreFrom(1, []beads.Bead{selfRoot}, nil)
		if isCorroboratedTeardownBead(store, selfRoot) {
			t.Fatal("a bead naming itself as root must not be corroborated")
		}
	})
	t.Run("dangling root fails", func(t *testing.T) {
		store := beads.NewMemStoreFrom(1, []beads.Bead{drain}, nil)
		if isCorroboratedTeardownBead(store, drain) {
			t.Fatal("a dangling root reference must not be corroborated")
		}
	})
	t.Run("closed root fails", func(t *testing.T) {
		closedRoot := root
		closedRoot.Status = "closed"
		store := beads.NewMemStoreFrom(1, []beads.Bead{closedRoot, drain}, nil)
		if isCorroboratedTeardownBead(store, drain) {
			t.Fatal("a closed root must not corroborate a teardown claim")
		}
	})
	t.Run("non-workflow root fails", func(t *testing.T) {
		taskRoot := beads.Bead{ID: rootID, Type: "task", Status: "open", Metadata: map[string]string{}}
		store := beads.NewMemStoreFrom(1, []beads.Bead{taskRoot, drain}, nil)
		if isCorroboratedTeardownBead(store, drain) {
			t.Fatal("a non-workflow root must not corroborate a teardown claim")
		}
	})
}

func TestValidWorkOutcome(t *testing.T) {
	for _, v := range []string{
		beadmeta.WorkOutcomeShipped, beadmeta.WorkOutcomeNoOp,
		beadmeta.WorkOutcomeBlocked, beadmeta.WorkOutcomeAbandoned,
	} {
		if !validWorkOutcome(v) {
			t.Errorf("validWorkOutcome(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "pass", "fail", "skipped", "done", "SHIPPED"} {
		if validWorkOutcome(v) {
			t.Errorf("validWorkOutcome(%q) = true, want false", v)
		}
	}
}

func TestWorkRecordCloseTargets(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantIDs []string
		wantOK  bool
	}{
		{"close subcommand", []string{"close", "wr-1"}, []string{"wr-1"}, true},
		{"close multiple", []string{"close", "wr-1", "wr-2"}, []string{"wr-1", "wr-2"}, true},
		{"update status=closed", []string{"update", "wr-1", "--status=closed"}, []string{"wr-1"}, true},
		{"update --status closed", []string{"update", "wr-1", "--status", "closed"}, []string{"wr-1"}, true},
		{"update -s closed", []string{"update", "wr-1", "-s", "closed"}, []string{"wr-1"}, true},
		{"last repeated status closes", []string{"update", "wr-1", "--status=open", "--status=closed"}, []string{"wr-1"}, true},
		{"last repeated status stays open", []string{"update", "wr-1", "--status=closed", "--status=open"}, nil, false},
		{"status-looking value is consumed", []string{"update", "wr-1", "--notes", "--status=open", "--status", "closed"}, []string{"wr-1"}, true},
		{"update to open is not a close", []string{"update", "wr-1", "--status=open"}, nil, false},
		{"update without status is not a close", []string{"update", "wr-1", "--notes", "x"}, nil, false},
		{"read subcommand is not a close", []string{"show", "wr-1"}, nil, false},
		{"empty args", nil, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ids, ok := workRecordCloseTargets(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (ids=%v)", ok, tc.wantOK, ids)
			}
			if strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

// TestEvaluateWorkRecordCloseGate exercises the full gate plumbing (store read,
// scoping, warn vs enforce fork) over an in-memory store, covering ADR-0009
// acceptance (b)/(c) at the integration level.
func TestEvaluateWorkRecordCloseGate(t *testing.T) {
	beadsList := []beads.Bead{
		{ID: "wr-shipped-nocommit", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped}},
		{ID: "wr-noop", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp}},
		{ID: "wr-atomic-noop", Type: "task", Status: "in_progress", Metadata: map[string]string{}},
		{ID: "wr-missing", Type: "task", Status: "in_progress", Metadata: map[string]string{}},
		{ID: "wr-control", Type: "task", Ref: "mol-demo", Status: "in_progress", Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}},
	}
	newStore := func() beads.Store { return beads.NewMemStoreFrom(1, beadsList, nil) }

	tests := []struct {
		name      string
		args      []string
		enforce   bool
		wantBlock bool
		wantWarn  string // substring expected on stderr; "" ⇒ no output
	}{
		{"non-close subcommand is ignored", []string{"show", "wr-shipped-nocommit"}, true, false, ""},
		{"control bead is exempt", []string{"close", "wr-control"}, true, false, ""},
		{"no-op close passes", []string{"close", "wr-noop"}, true, false, ""},
		{"shipped-no-commit warns only by default", []string{"close", "wr-shipped-nocommit"}, false, false, "work-record gate (warn-only)"},
		{"shipped-no-commit blocks when enforced", []string{"close", "wr-shipped-nocommit"}, true, true, "work-record gate (enforced)"},
		{"missing outcome blocks when enforced", []string{"close", "wr-missing"}, true, true, "missing " + beadmeta.WorkOutcomeMetadataKey},
		{"update --status=closed is gated", []string{"update", "wr-shipped-nocommit", "--status=closed"}, true, true, "close of wr-shipped-nocommit"},
		{
			"atomic update validates submitted metadata",
			[]string{"update", "wr-atomic-noop", "--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp, "--status=closed"},
			true,
			false,
			"",
		},
		{
			"metadata JSON validates submitted no-op",
			[]string{"update", "wr-missing", "--metadata", `{"gc.work_outcome":"no-op"}`, "--status=closed"},
			true,
			false,
			"",
		},
		{
			"metadata equals JSON validates submitted no-op",
			[]string{"update", "wr-missing", `--metadata={"gc.work_outcome":"no-op"}`, "--status=closed"},
			true,
			false,
			"",
		},
		{
			"last repeated metadata JSON value wins",
			[]string{"update", "wr-missing", `--metadata={"gc.work_outcome":"no-op"}`, `--metadata={"unrelated":"value"}`, "--status=closed"},
			true,
			true,
			"missing " + beadmeta.WorkOutcomeMetadataKey,
		},
		{
			"last repeated metadata JSON ignores an earlier malformed value",
			[]string{"update", "wr-missing", `--metadata={not-json}`, `--metadata={"gc.work_outcome":"no-op"}`, "--status=closed"},
			true,
			false,
			"",
		},
		{
			"metadata JSON cannot hide shipped evidence requirements behind stored no-op",
			[]string{"update", "wr-noop", `--metadata={"gc.work_outcome":"shipped"}`, "--status=closed"},
			true,
			true,
			beadmeta.WorkCommitMetadataKey,
		},
		{
			"metadata JSON cannot combine with later set-metadata",
			[]string{"update", "wr-noop", `--metadata={"gc.work_outcome":"shipped"}`, "--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp, "--status=closed"},
			true,
			true,
			"cannot project metadata",
		},
		{
			"metadata JSON cannot combine with earlier set-metadata",
			[]string{"update", "wr-noop", "--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp, `--metadata={"gc.work_outcome":"shipped"}`, "--status=closed"},
			true,
			true,
			"cannot project metadata",
		},
		{
			"unset-metadata wins over set-metadata regardless of argv order",
			[]string{"update", "wr-missing", "--unset-metadata", beadmeta.WorkOutcomeMetadataKey, "--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp, "--status=closed"},
			true,
			true,
			"missing " + beadmeta.WorkOutcomeMetadataKey,
		},
		{
			"metadata JSON cannot combine with unset-metadata",
			[]string{"update", "wr-noop", "--unset-metadata", beadmeta.WorkOutcomeMetadataKey, `--metadata={"gc.work_outcome":"no-op"}`, "--status=closed"},
			true,
			true,
			"cannot project metadata",
		},
		{
			"non-string metadata uses beads StringMap coercion",
			[]string{"update", "wr-noop", `--metadata={"gc.work_outcome":true}`, "--status=closed"},
			true,
			true,
			`invalid gc.work_outcome="true"`,
		},
		{
			"malformed metadata JSON fails closed",
			[]string{"update", "wr-noop", `--metadata={not-json}`, "--status=closed"},
			true,
			true,
			"cannot project --metadata",
		},
		{
			"metadata file input fails closed",
			[]string{"update", "wr-noop", "--metadata", "@work-record.json", "--status=closed"},
			true,
			true,
			"cannot project --metadata",
		},
		{
			"metadata-looking positional after terminator is not projected",
			[]string{"update", "wr-missing", "--status=closed", "--", "--set-metadata=" + beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp},
			true,
			true,
			"missing " + beadmeta.WorkOutcomeMetadataKey,
		},
		{
			"metadata-like flag value is not submitted metadata",
			[]string{"update", "wr-missing", "--notes", "--set-metadata=" + beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp, "--status=closed"},
			true,
			true,
			"missing " + beadmeta.WorkOutcomeMetadataKey,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			block := evaluateWorkRecordCloseGate(tc.args, newStore(), nil, t.TempDir(), tc.enforce, &stderr) != 0
			if block != tc.wantBlock {
				t.Fatalf("block = %v, want %v; stderr=%s", block, tc.wantBlock, stderr.String())
			}
			out := stderr.String()
			if tc.wantWarn == "" {
				if out != "" {
					t.Fatalf("expected no gate output, got %q", out)
				}
				return
			}
			if !strings.Contains(out, tc.wantWarn) {
				t.Fatalf("gate output %q does not contain %q", out, tc.wantWarn)
			}
		})
	}
}

// TestEvaluateWorkRecordCloseGateKindSpoof is the ga-w25b regression: a
// worker-claimable task cannot shed either close gate merely by carrying
// arbitrary, task, control, or cleanup gc.kind metadata. The atomic form also
// proves that stamping cleanup in the same update as close cannot change the
// classification performed on the stored bead.
func TestEvaluateWorkRecordCloseGateKindSpoof(t *testing.T) {
	const rootID = "wf-root"
	worked := func(id, kind string, extra map[string]string) beads.Bead {
		metadata := map[string]string{
			beadmeta.OutcomeMetadataKey:     beadmeta.OutcomePass,
			beadmeta.SessionIDMetadataKey:   "mc-spoof",
			beadmeta.SessionNameMetadataKey: "gc__implementation-worker-mc-spoof",
			beadmeta.RoutedToMetadataKey:    "gascity/gc.implementation-worker",
		}
		if kind != "" {
			metadata[beadmeta.KindMetadataKey] = kind
		}
		for k, v := range extra {
			metadata[k] = v
		}
		return beads.Bead{ID: id, Type: "task", Status: "in_progress", Metadata: metadata}
	}
	beadsList := []beads.Bead{
		{ID: rootID, Type: "task", Ref: "mol-demo", Status: "in_progress", Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}},
		{ID: "wf-real-drain", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindCleanup,
			beadmeta.RootBeadIDMetadataKey: rootID,
		}},
		worked("spoof-arbitrary", "totally-real-bookkeeping", nil),
		worked("spoof-task-kind", beadmeta.KindTask, nil),
		worked("spoof-control", beadmeta.KindCheck, nil),
		worked("spoof-cleanup", beadmeta.KindCleanup, map[string]string{beadmeta.RootBeadIDMetadataKey: rootID}),
		worked("spoof-atomic", "", nil),
	}
	closes := [][]string{
		{"close", "spoof-arbitrary"},
		{"close", "spoof-task-kind"},
		{"close", "spoof-control"},
		{"close", "spoof-cleanup"},
		{"update", "spoof-atomic", "--set-metadata", beadmeta.KindMetadataKey + "=" + beadmeta.KindCleanup, "--status=closed"},
	}
	for _, args := range closes {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			store := beads.NewMemStoreFrom(1, beadsList, nil)
			var stderr strings.Builder
			if code := evaluateWorkRecordCloseGate(args, store, nil, t.TempDir(), false, &stderr); code != 1 {
				t.Fatalf("exit code = %d, want 1 (blocked); stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "pass-close gate") {
				t.Fatalf("expected a pass-close refusal, got %q", stderr.String())
			}
		})
	}
}

// TestEvaluateWorkRecordCloseGateTeardownBookkeeping proves the genuine
// bookkeeping close shapes stay exempt end to end: corroborated mol-do-work
// and scoped teardown steps, plus convoy, message, and rollup containers, can
// close gc.outcome=pass without a work commit under enforcement.
func TestEvaluateWorkRecordCloseGateTeardownBookkeeping(t *testing.T) {
	const rootID = "wf-root"
	const attemptRootID = "wf-root-2"
	beadsList := []beads.Bead{
		{ID: rootID, Type: "task", Ref: "mol-demo", Status: "in_progress", Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}},
		{ID: "wf-do-work", Type: "task", Status: "closed", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID}},
		{ID: "wf-finalize", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindWorkflowFinalize,
			beadmeta.RootBeadIDMetadataKey: rootID,
			beadmeta.RoutedToMetadataKey:   "gascity/control-dispatcher",
		}},
		{ID: "wf-drain", Type: "task", Status: "in_progress", Metadata: map[string]string{
			beadmeta.KindMetadataKey:        beadmeta.KindCleanup,
			beadmeta.RootBeadIDMetadataKey:  rootID,
			beadmeta.OutcomeMetadataKey:     beadmeta.OutcomePass,
			beadmeta.SessionIDMetadataKey:   "mc-drain",
			beadmeta.SessionNameMetadataKey: "gc__implementation-worker-mc-drain",
			beadmeta.RoutedToMetadataKey:    "gascity/gc.implementation-worker",
			beadmeta.WorkerGenusMetadataKey: "anthropic",
		}},
		{ID: attemptRootID, Type: "task", Ref: "mol-attempt", Status: "in_progress", Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}},
		{ID: "wf2-body", Type: "task", Status: "closed", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: attemptRootID}},
		{ID: "wf2-retry", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindRetry,
			beadmeta.RootBeadIDMetadataKey: attemptRootID,
			beadmeta.RoutedToMetadataKey:   "gascity/control-dispatcher",
		}},
		{ID: "wf-teardown-attempt", Type: "task", Ref: "step.teardown", Status: "in_progress", Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindTask,
			beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleTeardown,
			beadmeta.RootBeadIDMetadataKey: attemptRootID,
			beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
			beadmeta.SessionIDMetadataKey:  "mc-attempt",
		}},
		{ID: "convoy-1", Type: "convoy", Status: "open", Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}},
		{ID: "mail-1", Type: "message", Status: "open"},
		{ID: "rollup-1", Type: "molecule", Status: "open", Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}},
	}
	for _, id := range []string{"wf-drain", "wf-teardown-attempt", "convoy-1", "mail-1", "rollup-1"} {
		t.Run(id, func(t *testing.T) {
			store := beads.NewMemStoreFrom(1, beadsList, nil)
			var stderr strings.Builder
			if code := evaluateWorkRecordCloseGate([]string{"close", id}, store, nil, t.TempDir(), true, &stderr); code != 0 {
				t.Fatalf("exit code = %d, want 0 (proceed); stderr=%s", code, stderr.String())
			}
			if out := stderr.String(); out != "" {
				t.Fatalf("expected no gate output for bookkeeping close, got %q", out)
			}
		})
	}
}

func TestEvaluateWorkRecordCloseGateAtomicShippedUpdate(t *testing.T) {
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	runGit(t, repoDir, "config", "user.name", "Gas City Test")
	runGit(t, repoDir, "config", "user.email", "gc-test@test.local")
	artifactPath := filepath.Join(repoDir, "artifact.txt")
	if err := os.WriteFile(artifactPath, []byte("integrated\n"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	runGit(t, repoDir, "add", "artifact.txt")
	runGit(t, repoDir, "commit", "-m", "test: integrate artifact")
	commit := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))

	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "wr-atomic-shipped",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey: repoDir,
		},
	}}, nil)
	args := []string{
		"update", "wr-atomic-shipped",
		"--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeShipped,
		"--set-metadata", beadmeta.WorkCommitMetadataKey + "=" + commit,
		"--set-metadata", beadmeta.WorkBranchMetadataKey + "=main",
		"--status=closed",
	}
	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate(args, store, nil, repoDir, true, &stderr); block != 0 {
		t.Fatalf("valid atomic shipped close blocked; stderr=%s", stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("valid atomic shipped close warned: %q", got)
	}
}

// panicOnGetStore embeds a nil beads.Store and overrides Get to panic. It
// proves a code path never falls back to the store for a given ID — used to
// assert the close gate actually consumes preFetched beads instead of
// re-reading them: gc bd close previously paid for the same store.Get twice,
// once in the write-ID guard and once in this gate.
type panicOnGetStore struct{ beads.Store }

func (panicOnGetStore) Get(id string) (beads.Bead, error) {
	panic("store.Get called for id " + id + ": preFetched bead should have been used")
}

func TestEvaluateWorkRecordCloseGateUsesPreFetchedBead(t *testing.T) {
	preFetched := map[string]beads.Bead{
		"wr-shipped-nocommit": {ID: "wr-shipped-nocommit", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped}},
	}
	var stderr strings.Builder
	block := evaluateWorkRecordCloseGate([]string{"close", "wr-shipped-nocommit"}, panicOnGetStore{}, preFetched, t.TempDir(), true, &stderr) != 0
	if !block {
		t.Fatalf("expected block=true for shipped-without-commit, got false; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "work-record gate (enforced)") {
		t.Fatalf("expected enforced gate output, got %q", stderr.String())
	}
}

// TestRunWorkRecordCloseGateReusesPreOpenedStore proves runWorkRecordCloseGate
// never calls openStoreAtForCity when handed a preOpened store — it's the IO
// wrapper's half of the dedup (evaluateWorkRecordCloseGate proves the
// preFetched-bead half above). cityPath is deliberately bogus: a gate that
// silently dropped preOpened/preFetched would fall back to opening or reading
// a real store there and fail, producing an unverifiable-close refusal
// ("pass-close gate ... cannot load the bead") instead of the work-record
// violation the pre-read bead carries. Asserting the enforced work-record
// violation fires proves the handed-in store and beads were actually used.
func TestRunWorkRecordCloseGateReusesPreOpenedStore(t *testing.T) {
	preFetched := map[string]beads.Bead{
		"wr-shipped-nocommit": {ID: "wr-shipped-nocommit", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped}},
	}
	var stderr strings.Builder
	const bogusCityPath = "/nonexistent/does-not-exist"
	t.Setenv(workRecordEnforceEnvVar, "1")
	block := runWorkRecordCloseGate([]string{"close", "wr-shipped-nocommit"}, t.TempDir(), bogusCityPath, nil, panicOnGetStore{}, preFetched, &stderr) != 0
	if !block {
		t.Fatalf("expected block=true for shipped-without-commit, got false (fallback store open may have silently swallowed the preOpened store); stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "work-record gate (enforced)") {
		t.Fatalf("expected enforced gate output, got %q", stderr.String())
	}
}

func TestPassCloseDispositionFromArgs(t *testing.T) {
	outcomeKey := beadmeta.OutcomeMetadataKey
	tests := []struct {
		name string
		args []string
		want passCloseArgvDisposition
	}{
		{"bare close is undecided", []string{"close", "wr-x"}, passCloseArgvUndecided},
		{"close with reason is undecided", []string{"close", "wr-x", "--reason", "done"}, passCloseArgvUndecided},
		{"update without outcome edit is undecided", []string{"update", "wr-x", "--status=closed"}, passCloseArgvUndecided},
		{"set pass is pass", []string{"update", "wr-x", "--set-metadata", outcomeKey + "=" + beadmeta.OutcomePass, "--status=closed"}, passCloseArgvPass},
		{"set fail is non-pass", []string{"update", "wr-x", "--set-metadata", outcomeKey + "=" + beadmeta.OutcomeFail, "--status=closed"}, passCloseArgvNonPass},
		{"last repeated set wins toward non-pass", []string{"update", "wr-x", "--set-metadata", outcomeKey + "=" + beadmeta.OutcomePass, "--set-metadata", outcomeKey + "=" + beadmeta.OutcomeFail, "--status=closed"}, passCloseArgvNonPass},
		{"last repeated set wins toward pass", []string{"update", "wr-x", "--set-metadata", outcomeKey + "=" + beadmeta.OutcomeFail, "--set-metadata", outcomeKey + "=" + beadmeta.OutcomePass, "--status=closed"}, passCloseArgvPass},
		{"unset beats set regardless of argv order", []string{"update", "wr-x", "--unset-metadata", outcomeKey, "--set-metadata", outcomeKey + "=" + beadmeta.OutcomePass, "--status=closed"}, passCloseArgvNonPass},
		{"metadata JSON stamping pass is pass", []string{"update", "wr-x", "--metadata", `{"gc.outcome":"pass"}`, "--status=closed"}, passCloseArgvPass},
		{"metadata JSON stamping fail is non-pass", []string{"update", "wr-x", "--metadata", `{"gc.outcome":"fail"}`, "--status=closed"}, passCloseArgvNonPass},
		{"additive metadata JSON without the key is undecided", []string{"update", "wr-x", "--metadata", `{"other":"v"}`, "--status=closed"}, passCloseArgvUndecided},
		{"malformed metadata JSON is undecided", []string{"update", "wr-x", "--metadata", "{not-json}", "--status=closed"}, passCloseArgvUndecided},
		{"metadata file input is undecided", []string{"update", "wr-x", "--metadata", "@close.json", "--status=closed"}, passCloseArgvUndecided},
		{"metadata JSON combined with set-metadata is undecided", []string{"update", "wr-x", "--metadata", `{"gc.outcome":"fail"}`, "--set-metadata", "a=b", "--status=closed"}, passCloseArgvUndecided},
		{"outcome-looking positional after terminator is undecided", []string{"update", "wr-x", "--status=closed", "--", "--set-metadata=" + outcomeKey + "=" + beadmeta.OutcomeFail}, passCloseArgvUndecided},
		{"outcome-looking flag value is undecided", []string{"update", "wr-x", "--notes", "--set-metadata=" + outcomeKey + "=" + beadmeta.OutcomeFail, "--status=closed"}, passCloseArgvUndecided},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := passCloseDispositionFromArgs(tc.args); got != tc.want {
				t.Fatalf("passCloseDispositionFromArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// brokenStoreCity returns a city path whose store open deterministically
// fails: its city.toml names the removed sqlite provider, which
// openStoreResultAtForCityWithConfig rejects with a hard error before any
// store is constructed (the same fixture as
// TestOpenStoreResultAtForCityRejectsRemovedSQLiteProvider).
func brokenStoreCity(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	cityToml := "[workspace]\nname = \"broken-store-city\"\nprefix = \"ga\"\n\n[beads]\nprovider = \"sqlite\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

// TestRunWorkRecordCloseGateFailsClosedWithoutStore proves the store-open
// failure path (ga-c6sz): when the gate cannot open its store, a close that
// may carry gc.outcome=pass — a bare close of a bead whose outcome was
// stamped earlier, or an atomic update stamping pass — is refused with an
// actionable error, while a close that pins a non-pass outcome in its own
// argv proceeds (failure bookkeeping must survive a read-path outage).
// preOpened/preFetched are nil, matching production when the write-ID
// guard's open also failed.
func TestRunWorkRecordCloseGateFailsClosedWithoutStore(t *testing.T) {
	t.Run("bare close is refused", func(t *testing.T) {
		cityDir := brokenStoreCity(t)
		var stderr strings.Builder
		block := runWorkRecordCloseGate([]string{"close", "wr-unverified"}, cityDir, cityDir, nil, nil, nil, &stderr) != 0
		if !block {
			t.Fatalf("expected block=true for unverifiable bare close, got false; stderr=%s", stderr.String())
		}
		for _, want := range []string{"pass-close gate", "refusing close of wr-unverified", "cannot load the bead", "opening beads store", "no longer supported"} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr %q does not contain %q", stderr.String(), want)
			}
		}
	})

	t.Run("atomic pass close is refused", func(t *testing.T) {
		cityDir := brokenStoreCity(t)
		args := []string{"update", "wr-unverified", "--set-metadata", beadmeta.OutcomeMetadataKey + "=" + beadmeta.OutcomePass, "--status=closed"}
		var stderr strings.Builder
		if block := runWorkRecordCloseGate(args, cityDir, cityDir, nil, nil, nil, &stderr); block == 0 {
			t.Fatalf("expected block=true for unverifiable atomic pass close, got false; stderr=%s", stderr.String())
		}
	})

	t.Run("atomic non-pass close proceeds with a skip note", func(t *testing.T) {
		cityDir := brokenStoreCity(t)
		args := []string{"update", "wr-unverified", "--set-metadata", beadmeta.OutcomeMetadataKey + "=" + beadmeta.OutcomeFail, "--status=closed"}
		var stderr strings.Builder
		if block := runWorkRecordCloseGate(args, cityDir, cityDir, nil, nil, nil, &stderr); block != 0 {
			t.Fatalf("non-pass close blocked on store-open failure; stderr=%s", stderr.String())
		}
		for _, want := range []string{"skipping validation", "non-pass"} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr %q does not contain %q", stderr.String(), want)
			}
		}
	})

	t.Run("non-close invocation stays untouched", func(t *testing.T) {
		cityDir := brokenStoreCity(t)
		var stderr strings.Builder
		if block := runWorkRecordCloseGate([]string{"show", "wr-unverified"}, cityDir, cityDir, nil, nil, nil, &stderr); block != 0 {
			t.Fatalf("non-close invocation blocked; stderr=%s", stderr.String())
		}
		if stderr.String() != "" {
			t.Fatalf("non-close invocation produced gate output: %q", stderr.String())
		}
	})
}

// erroringGetStore fails every Get with a fixed error, simulating an
// unhealthy or lagging read seam behind an already-open store handle.
type erroringGetStore struct {
	beads.Store
	err error
}

func (s erroringGetStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

// TestEvaluateWorkRecordCloseGateFailsClosedOnUnreadableBead proves the
// store.Get failure path (ga-c6sz): a close target the gate cannot read is
// refused unless the invocation itself pins a non-pass gc.outcome. This
// covers both a transient read error and ErrNotFound (projection lag: the
// stamp-then-close pass form writes gc.outcome=pass through the write seam
// moments before the close, so a lagging read seam is exactly when the pass
// contract would otherwise be bypassed). Enforcement mode must not matter —
// the pass contract is enforced independently of GC_WORK_RECORD_ENFORCE.
func TestEvaluateWorkRecordCloseGateFailsClosedOnUnreadableBead(t *testing.T) {
	readErr := errors.New("dolt read seam unavailable")

	t.Run("bare close on erroring store is refused", func(t *testing.T) {
		for _, enforce := range []bool{false, true} {
			var stderr strings.Builder
			block := evaluateWorkRecordCloseGate([]string{"close", "wr-lagged"}, erroringGetStore{err: readErr}, nil, t.TempDir(), enforce, &stderr) != 0
			if !block {
				t.Fatalf("enforce=%v: expected block=true for unreadable close target, got false; stderr=%s", enforce, stderr.String())
			}
			for _, want := range []string{"refusing close of wr-lagged", "dolt read seam unavailable", "retry when the beads read path is healthy"} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("enforce=%v: stderr %q does not contain %q", enforce, stderr.String(), want)
				}
			}
		}
	})

	t.Run("bare close on missing bead is refused", func(t *testing.T) {
		var stderr strings.Builder
		block := evaluateWorkRecordCloseGate([]string{"close", "wr-lagged"}, beads.NewMemStoreFrom(1, nil, nil), nil, t.TempDir(), false, &stderr) != 0
		if !block {
			t.Fatalf("expected block=true for close of a bead the read seam cannot see, got false; stderr=%s", stderr.String())
		}
	})

	t.Run("atomic pass close on erroring store is refused", func(t *testing.T) {
		args := []string{"update", "wr-lagged", "--set-metadata", beadmeta.OutcomeMetadataKey + "=" + beadmeta.OutcomePass, "--status=closed"}
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate(args, erroringGetStore{err: readErr}, nil, t.TempDir(), false, &stderr); block == 0 {
			t.Fatalf("expected block=true for unreadable atomic pass close, got false; stderr=%s", stderr.String())
		}
	})

	t.Run("atomic non-pass close on erroring store proceeds with a skip note", func(t *testing.T) {
		args := []string{"update", "wr-lagged", "--set-metadata", beadmeta.OutcomeMetadataKey + "=" + beadmeta.OutcomeFail, "--status=closed"}
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate(args, erroringGetStore{err: readErr}, nil, t.TempDir(), false, &stderr); block != 0 {
			t.Fatalf("non-pass close blocked on unreadable bead; stderr=%s", stderr.String())
		}
		if !strings.Contains(stderr.String(), "skipping validation") {
			t.Fatalf("stderr %q does not contain the skip note", stderr.String())
		}
	})

	t.Run("preFetched bead still bypasses the failing store", func(t *testing.T) {
		preFetched := map[string]beads.Bead{
			"wr-lagged": {ID: "wr-lagged", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp}},
		}
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate([]string{"close", "wr-lagged"}, erroringGetStore{err: readErr}, preFetched, t.TempDir(), false, &stderr); block != 0 {
			t.Fatalf("valid no-op close blocked despite preFetched bead; stderr=%s", stderr.String())
		}
	})
}

// TestEvaluateWorkRecordCloseGateSilentFallbackKeepsLoudContract proves the
// gate does not mask bd's silent-fallback loud-error contract (#2079/#2080):
// when the read seam reports ErrBDSilentFallback, the close is refused with
// bdSilentFallbackExitCode and the operator-facing message — not the generic
// exit-1 unverifiable-close refusal — regardless of the close's argv
// disposition, since nothing written in fallback mode persists.
func TestEvaluateWorkRecordCloseGateSilentFallbackKeepsLoudContract(t *testing.T) {
	fallbackErr := fmt.Errorf("getting bead %q: %w: auto-importing 220929 bytes", "wr-fb", beads.ErrBDSilentFallback)
	shapes := map[string][]string{
		"bare close":            {"close", "wr-fb"},
		"atomic non-pass close": {"update", "wr-fb", "--set-metadata", beadmeta.OutcomeMetadataKey + "=" + beadmeta.OutcomeFail, "--status=closed"},
		"atomic pass close":     {"update", "wr-fb", "--set-metadata", beadmeta.OutcomeMetadataKey + "=" + beadmeta.OutcomePass, "--status=closed"},
	}
	for name, args := range shapes {
		t.Run(name, func(t *testing.T) {
			var stderr strings.Builder
			code := evaluateWorkRecordCloseGate(args, erroringGetStore{err: fallbackErr}, nil, t.TempDir(), false, &stderr)
			if code != bdSilentFallbackExitCode {
				t.Fatalf("code = %d, want %d (silent-fallback exit code); stderr=%s", code, bdSilentFallbackExitCode, stderr.String())
			}
			for _, want := range []string{"managed Dolt unreachable", "auto-importing", "close of wr-fb"} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr %q does not contain %q", stderr.String(), want)
				}
			}
		})
	}
}

// TestEvaluateWorkRecordCloseGateProjectionErrorAlwaysBlocks proves a close
// whose final metadata cannot be computed is refused independently of the
// migration-mode enforce switch: @file metadata is the one unprojectable
// shape bd itself accepts, so riding the warn-only default would let an
// unverified — possibly pass — close through (ga-c6sz).
func TestEvaluateWorkRecordCloseGateProjectionErrorAlwaysBlocks(t *testing.T) {
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "wr-file-pass", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}},
		{ID: "wr-file-plain", Type: "task", Status: "in_progress", Metadata: map[string]string{}},
	}, nil)
	for _, id := range []string{"wr-file-pass", "wr-file-plain"} {
		args := []string{"update", id, "--metadata", "@close.json", "--status=closed"}
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate(args, store, nil, t.TempDir(), false, &stderr); block == 0 {
			t.Fatalf("close of %s with unprojectable metadata not blocked in warn-only mode; stderr=%s", id, stderr.String())
		}
		for _, want := range []string{"cannot project --metadata", "refusing close of " + id} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr %q does not contain %q", stderr.String(), want)
			}
		}
	}
}

// passingPassCloseChecks returns injected pass-close predicates describing a
// fully healthy repo state: the commit exists on a branch and the tree is
// clean. Tests override individual fields to simulate each violation.
func passingPassCloseChecks() passCloseChecks {
	return passCloseChecks{
		commitExists:      func(string) bool { return true },
		commitOnAnyBranch: func(string) bool { return true },
		dirtyPaths:        func() ([]string, error) { return nil, nil },
	}
}

// failingPassCloseChecks returns predicates that fail every check, used to
// prove non-pass outcomes never consult the repo at all.
func failingPassCloseChecks() passCloseChecks {
	return passCloseChecks{
		commitExists:      func(string) bool { return false },
		commitOnAnyBranch: func(string) bool { return false },
		dirtyPaths:        func() ([]string, error) { return []string{"wip.txt"}, nil },
	}
}

func TestValidatePassCloseOnClose(t *testing.T) {
	tests := []struct {
		name      string
		meta      map[string]string
		checks    passCloseChecks
		wantViols []string // substring per expected violation, in order; empty ⇒ none
	}{
		{
			name:      "no outcome is exempt",
			meta:      map[string]string{},
			checks:    failingPassCloseChecks(),
			wantViols: nil,
		},
		{
			name:      "fail outcome is exempt",
			meta:      map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail},
			checks:    failingPassCloseChecks(),
			wantViols: nil,
		},
		{
			name:      "skipped outcome is exempt",
			meta:      map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped},
			checks:    failingPassCloseChecks(),
			wantViols: nil,
		},
		{
			name: "pass with reachable commit and clean tree passes",
			meta: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: "abc123",
			},
			checks:    passingPassCloseChecks(),
			wantViols: nil,
		},
		{
			name:      "pass without work commit is refused",
			meta:      map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass},
			checks:    passingPassCloseChecks(),
			wantViols: []string{"requires " + beadmeta.WorkCommitMetadataKey},
		},
		{
			name: "pass with nonexistent commit is refused",
			meta: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: "deadbeef",
			},
			checks: func() passCloseChecks {
				c := passingPassCloseChecks()
				c.commitExists = func(string) bool { return false }
				return c
			}(),
			wantViols: []string{"does not exist"},
		},
		{
			name: "pass with detached-HEAD-only commit is refused",
			meta: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: "abc123",
			},
			checks: func() passCloseChecks {
				c := passingPassCloseChecks()
				c.commitOnAnyBranch = func(string) bool { return false }
				return c
			}(),
			wantViols: []string{"not reachable from any branch"},
		},
		{
			name: "pass with dirty non-infrastructure tree is refused",
			meta: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: "abc123",
			},
			checks: func() passCloseChecks {
				c := passingPassCloseChecks()
				c.dirtyPaths = func() ([]string, error) { return []string{".agents/state.json", "internal/foo.go"}, nil }
				return c
			}(),
			wantViols: []string{"internal/foo.go"},
		},
		{
			name: "no-op work outcome needs no commit",
			meta: map[string]string{
				beadmeta.OutcomeMetadataKey:     beadmeta.OutcomePass,
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp,
			},
			checks: func() passCloseChecks {
				// Commit predicates fail so the test proves they are never
				// consulted when a typed no-op close carries no commit.
				c := failingPassCloseChecks()
				c.dirtyPaths = func() ([]string, error) { return nil, nil }
				return c
			}(),
			wantViols: nil,
		},
		{
			name: "no-op work outcome with dirty tracked tree is refused",
			meta: map[string]string{
				beadmeta.OutcomeMetadataKey:     beadmeta.OutcomePass,
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp,
			},
			checks: func() passCloseChecks {
				c := passingPassCloseChecks()
				c.dirtyPaths = func() ([]string, error) { return []string{"internal/foo.go"}, nil }
				return c
			}(),
			wantViols: []string{"internal/foo.go"},
		},
		{
			name: "no-op with a stamped commit still validates it",
			meta: map[string]string{
				beadmeta.OutcomeMetadataKey:     beadmeta.OutcomePass,
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp,
				beadmeta.WorkCommitMetadataKey:  "deadbeef",
			},
			checks: func() passCloseChecks {
				c := passingPassCloseChecks()
				c.commitExists = func(string) bool { return false }
				return c
			}(),
			wantViols: []string{"does not exist"},
		},
		{
			name: "shipped work outcome still requires a commit",
			meta: map[string]string{
				beadmeta.OutcomeMetadataKey:     beadmeta.OutcomePass,
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
			},
			checks:    passingPassCloseChecks(),
			wantViols: []string{"requires " + beadmeta.WorkCommitMetadataKey},
		},
		{
			name: "infrastructure-only dirt is clean",
			meta: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: "abc123",
			},
			checks: func() passCloseChecks {
				c := passingPassCloseChecks()
				c.dirtyPaths = func() ([]string, error) {
					return []string{".agents/state.json", ".codex/session.log", ".gc/", ".gc/tmp/x"}, nil
				}
				return c
			}(),
			wantViols: nil,
		},
		{
			name: "unverifiable tree is refused",
			meta: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: "abc123",
			},
			checks: func() passCloseChecks {
				c := passingPassCloseChecks()
				c.dirtyPaths = func() ([]string, error) { return nil, os.ErrPermission }
				return c
			}(),
			wantViols: []string{"cannot verify"},
		},
		{
			name: "missing commit and dirty tree report both violations",
			meta: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass},
			checks: func() passCloseChecks {
				c := passingPassCloseChecks()
				c.dirtyPaths = func() ([]string, error) { return []string{"wip.txt"}, nil }
				return c
			}(),
			wantViols: []string{"requires " + beadmeta.WorkCommitMetadataKey, "wip.txt"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bead := beads.Bead{ID: "pc-1", Type: "task", Metadata: tc.meta}
			got := validatePassCloseOnClose(bead, tc.checks)
			if len(got) != len(tc.wantViols) {
				t.Fatalf("got %d violations %v, want %d matching %v", len(got), got, len(tc.wantViols), tc.wantViols)
			}
			for i, want := range tc.wantViols {
				if !strings.Contains(got[i], want) {
					t.Fatalf("violation[%d] %q does not contain %q", i, got[i], want)
				}
			}
		})
	}
}

func TestIsPassCloseInfraPath(t *testing.T) {
	for _, p := range []string{".agents", ".agents/state.json", ".codex/session.log", ".gc", ".gc/", ".gc/tmp/x", "./.gc/tmp"} {
		if !isPassCloseInfraPath(p) {
			t.Errorf("isPassCloseInfraPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"", "foo.txt", "agents/x", ".gcx/foo", ".agentsx", "sub/.gc/x", "internal/foo.go"} {
		if isPassCloseInfraPath(p) {
			t.Errorf("isPassCloseInfraPath(%q) = true, want false", p)
		}
	}
}

// newPassCloseRepo creates a git repo with one commit on main and returns its
// path and that commit's SHA.
func newPassCloseRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--initial-branch=main")
	runGit(t, dir, "config", "user.name", "Gas City Test")
	runGit(t, dir, "config", "user.email", "gc-test@test.local")
	if err := os.WriteFile(filepath.Join(dir, "artifact.txt"), []byte("landed\n"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	runGit(t, dir, "add", "artifact.txt")
	runGit(t, dir, "commit", "-m", "test: land artifact")
	return dir, strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
}

// TestGitDirtyWorkTreePaths pins the tracked-content contract of the dirty
// scan: modifications to tracked files (worktree or index, including renames)
// are dirt; untracked and gitignored paths are not. A real rig accumulates
// hundreds of untracked runtime paths (.claude/, worktrees/, local backups —
// ga-vx36) that are not evidence of an uncommitted diff.
func TestGitDirtyWorkTreePaths(t *testing.T) {
	t.Run("tracked modifications count, untracked and ignored do not", func(t *testing.T) {
		repoDir, _ := newPassCloseRepo(t)
		if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("scratch/\n"), 0o644); err != nil {
			t.Fatalf("write gitignore: %v", err)
		}
		runGit(t, repoDir, "add", ".gitignore")
		runGit(t, repoDir, "commit", "-m", "test: ignore scratch")
		// Tracked modification in the worktree.
		if err := os.WriteFile(filepath.Join(repoDir, "artifact.txt"), []byte("modified\n"), 0o644); err != nil {
			t.Fatalf("modify artifact: %v", err)
		}
		// Staged-but-uncommitted new file (tracked in the index).
		if err := os.WriteFile(filepath.Join(repoDir, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
			t.Fatalf("write staged: %v", err)
		}
		runGit(t, repoDir, "add", "staged.txt")
		// Untracked file, untracked runtime directory, gitignored file.
		if err := os.WriteFile(filepath.Join(repoDir, "stray.txt"), []byte("stray\n"), 0o644); err != nil {
			t.Fatalf("write stray: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(repoDir, ".claude"), 0o755); err != nil {
			t.Fatalf("mkdir .claude: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, ".claude", "settings.json"), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write .claude settings: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(repoDir, "scratch"), 0o755); err != nil {
			t.Fatalf("mkdir scratch: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, "scratch", "tmp.txt"), []byte("scratch\n"), 0o644); err != nil {
			t.Fatalf("write scratch: %v", err)
		}

		paths, err := gitDirtyWorkTreePaths(repoDir)
		if err != nil {
			t.Fatalf("gitDirtyWorkTreePaths: %v", err)
		}
		got := make(map[string]bool, len(paths))
		for _, p := range paths {
			got[p] = true
		}
		for _, want := range []string{"artifact.txt", "staged.txt"} {
			if !got[want] {
				t.Errorf("tracked change %q missing from dirty paths %v", want, paths)
			}
		}
		for _, exclude := range []string{"stray.txt", ".claude/", ".claude/settings.json", "scratch/", "scratch/tmp.txt"} {
			if got[exclude] {
				t.Errorf("untracked/ignored path %q reported as dirt in %v", exclude, paths)
			}
		}
	})

	t.Run("renames carry both sides", func(t *testing.T) {
		repoDir, _ := newPassCloseRepo(t)
		runGit(t, repoDir, "mv", "artifact.txt", "renamed.txt")
		paths, err := gitDirtyWorkTreePaths(repoDir)
		if err != nil {
			t.Fatalf("gitDirtyWorkTreePaths: %v", err)
		}
		got := make(map[string]bool, len(paths))
		for _, p := range paths {
			got[p] = true
		}
		for _, want := range []string{"renamed.txt", "artifact.txt"} {
			if !got[want] {
				t.Errorf("rename side %q missing from dirty paths %v", want, paths)
			}
		}
	})
}

// TestEvaluateWorkRecordCloseGatePassClose exercises the pass-close gate
// end-to-end against real git repos. Every case runs with enforce=false: the
// pass-close contract blocks unconditionally, independent of the warn-only
// work-record migration default (GC_WORK_RECORD_ENFORCE).
func TestEvaluateWorkRecordCloseGatePassClose(t *testing.T) {
	t.Run("clean reachable pass close is allowed", func(t *testing.T) {
		repoDir, commit := newPassCloseRepo(t)
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-ok", Type: "task", Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: commit,
			},
		}}, nil)
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate([]string{"close", "pc-ok"}, store, nil, repoDir, false, &stderr); block != 0 {
			t.Fatalf("clean pass close blocked; stderr=%s", stderr.String())
		}
		if strings.Contains(stderr.String(), "pass-close gate") {
			t.Fatalf("unexpected pass-close gate output: %q", stderr.String())
		}
	})

	t.Run("pass close without work commit is refused", func(t *testing.T) {
		repoDir, _ := newPassCloseRepo(t)
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-nocommit", Type: "task", Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkBranchMetadataKey: "main",
			},
		}}, nil)
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate([]string{"close", "pc-nocommit"}, store, nil, repoDir, false, &stderr); block == 0 {
			t.Fatalf("pass close without commit not blocked; stderr=%s", stderr.String())
		}
		for _, want := range []string{"pass-close gate", "requires " + beadmeta.WorkCommitMetadataKey, "commit the work"} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr %q does not contain %q", stderr.String(), want)
			}
		}
	})

	t.Run("detached-HEAD-only commit is refused", func(t *testing.T) {
		repoDir, _ := newPassCloseRepo(t)
		runGit(t, repoDir, "checkout", "--detach")
		if err := os.WriteFile(filepath.Join(repoDir, "detached.txt"), []byte("stranded\n"), 0o644); err != nil {
			t.Fatalf("write detached artifact: %v", err)
		}
		runGit(t, repoDir, "add", "detached.txt")
		runGit(t, repoDir, "commit", "-m", "test: stranded on detached HEAD")
		detached := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-detached", Type: "task", Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: detached,
			},
		}}, nil)
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate([]string{"close", "pc-detached"}, store, nil, repoDir, false, &stderr); block == 0 {
			t.Fatalf("detached-HEAD pass close not blocked; stderr=%s", stderr.String())
		}
		if !strings.Contains(stderr.String(), "not reachable from any branch") {
			t.Fatalf("stderr %q does not mention branch reachability", stderr.String())
		}
	})

	t.Run("nonexistent commit is refused", func(t *testing.T) {
		repoDir, _ := newPassCloseRepo(t)
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-bogus", Type: "task", Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			},
		}}, nil)
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate([]string{"close", "pc-bogus"}, store, nil, repoDir, false, &stderr); block == 0 {
			t.Fatalf("bogus-commit pass close not blocked; stderr=%s", stderr.String())
		}
		if !strings.Contains(stderr.String(), "does not exist") {
			t.Fatalf("stderr %q does not mention commit existence", stderr.String())
		}
	})

	t.Run("modified tracked file is refused", func(t *testing.T) {
		repoDir, commit := newPassCloseRepo(t)
		if err := os.WriteFile(filepath.Join(repoDir, "artifact.txt"), []byte("uncommitted edit\n"), 0o644); err != nil {
			t.Fatalf("modify artifact: %v", err)
		}
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-dirty", Type: "task", Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: commit,
			},
		}}, nil)
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate([]string{"close", "pc-dirty"}, store, nil, repoDir, false, &stderr); block == 0 {
			t.Fatalf("dirty-tree pass close not blocked; stderr=%s", stderr.String())
		}
		if !strings.Contains(stderr.String(), "artifact.txt") {
			t.Fatalf("stderr %q does not name the dirty path", stderr.String())
		}
	})

	t.Run("untracked files never block a pass close", func(t *testing.T) {
		repoDir, commit := newPassCloseRepo(t)
		// Ambient rig noise: stray files and runtime directories that are
		// untracked but not gitignored (the Podcast-Updates shape, ga-vx36).
		if err := os.WriteFile(filepath.Join(repoDir, "stray.txt"), []byte("stray\n"), 0o644); err != nil {
			t.Fatalf("write stray: %v", err)
		}
		for _, dir := range []string{".claude", "worktrees/x", ".beads.local-backup"} {
			if err := os.MkdirAll(filepath.Join(repoDir, dir), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
			if err := os.WriteFile(filepath.Join(repoDir, dir, "f.txt"), []byte("runtime\n"), 0o644); err != nil {
				t.Fatalf("write %s file: %v", dir, err)
			}
		}
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-untracked", Type: "task", Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: commit,
			},
		}}, nil)
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate([]string{"close", "pc-untracked"}, store, nil, repoDir, false, &stderr); block != 0 {
			t.Fatalf("untracked-only dirt blocked the close; stderr=%s", stderr.String())
		}
	})

	t.Run("atomic no-op close without commit is allowed", func(t *testing.T) {
		repoDir, _ := newPassCloseRepo(t)
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-noop", Type: "task", Status: "in_progress", Metadata: map[string]string{},
		}}, nil)
		args := []string{
			"update", "pc-noop",
			"--set-metadata", beadmeta.OutcomeMetadataKey + "=" + beadmeta.OutcomePass,
			"--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp,
			"--status=closed",
		}
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate(args, store, nil, repoDir, false, &stderr); block != 0 {
			t.Fatalf("typed no-op pass close blocked; stderr=%s", stderr.String())
		}
	})

	t.Run("infrastructure-only dirt is allowed", func(t *testing.T) {
		repoDir, commit := newPassCloseRepo(t)
		for _, dir := range []string{".agents", ".codex", ".gc"} {
			if err := os.MkdirAll(filepath.Join(repoDir, dir), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
			if err := os.WriteFile(filepath.Join(repoDir, dir, "state.json"), []byte("{}\n"), 0o644); err != nil {
				t.Fatalf("write %s state: %v", dir, err)
			}
		}
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-infra", Type: "task", Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: commit,
			},
		}}, nil)
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate([]string{"close", "pc-infra"}, store, nil, repoDir, false, &stderr); block != 0 {
			t.Fatalf("infrastructure-only dirt blocked the close; stderr=%s", stderr.String())
		}
	})

	t.Run("gitignored dirt is clean", func(t *testing.T) {
		repoDir, _ := newPassCloseRepo(t)
		if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("scratch/\n"), 0o644); err != nil {
			t.Fatalf("write gitignore: %v", err)
		}
		runGit(t, repoDir, "add", ".gitignore")
		runGit(t, repoDir, "commit", "-m", "test: ignore scratch")
		commit := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))
		if err := os.MkdirAll(filepath.Join(repoDir, "scratch"), 0o755); err != nil {
			t.Fatalf("mkdir scratch: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, "scratch", "tmp.txt"), []byte("scratch\n"), 0o644); err != nil {
			t.Fatalf("write scratch: %v", err)
		}
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-ignored", Type: "task", Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.WorkCommitMetadataKey: commit,
			},
		}}, nil)
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate([]string{"close", "pc-ignored"}, store, nil, repoDir, false, &stderr); block != 0 {
			t.Fatalf("gitignored dirt blocked the close; stderr=%s", stderr.String())
		}
	})

	t.Run("control bead with pass outcome is exempt", func(t *testing.T) {
		repoDir, _ := newPassCloseRepo(t)
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-control", Type: "task", Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:     beadmeta.KindCheck,
				beadmeta.RoutedToMetadataKey: "gascity/control-dispatcher",
				beadmeta.OutcomeMetadataKey:  beadmeta.OutcomePass,
			},
		}}, nil)
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate([]string{"close", "pc-control"}, store, nil, repoDir, false, &stderr); block != 0 {
			t.Fatalf("control bead pass close blocked; stderr=%s", stderr.String())
		}
	})

	t.Run("atomic update stamping pass without commit is refused", func(t *testing.T) {
		repoDir, _ := newPassCloseRepo(t)
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-atomic", Type: "task", Status: "in_progress", Metadata: map[string]string{},
		}}, nil)
		args := []string{
			"update", "pc-atomic",
			"--set-metadata", beadmeta.OutcomeMetadataKey + "=" + beadmeta.OutcomePass,
			"--status=closed",
		}
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate(args, store, nil, repoDir, false, &stderr); block == 0 {
			t.Fatalf("atomic pass close without commit not blocked; stderr=%s", stderr.String())
		}
	})

	t.Run("atomic update stamping pass with commit resolves work dir metadata", func(t *testing.T) {
		repoDir, commit := newPassCloseRepo(t)
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID: "pc-atomic-ok", Type: "task", Status: "in_progress",
			Metadata: map[string]string{beadmeta.WorkDirMetadataKey: repoDir},
		}}, nil)
		args := []string{
			"update", "pc-atomic-ok",
			"--set-metadata", beadmeta.OutcomeMetadataKey + "=" + beadmeta.OutcomePass,
			"--set-metadata", beadmeta.WorkCommitMetadataKey + "=" + commit,
			"--status=closed",
		}
		var stderr strings.Builder
		if block := evaluateWorkRecordCloseGate(args, store, nil, t.TempDir(), false, &stderr); block != 0 {
			t.Fatalf("valid atomic pass close blocked; stderr=%s", stderr.String())
		}
	})
}

func TestWorkRecordEnforceEnabled(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv(workRecordEnforceEnvVar, v)
		if !workRecordEnforceEnabled() {
			t.Errorf("workRecordEnforceEnabled(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "off", "nope"} {
		t.Setenv(workRecordEnforceEnvVar, v)
		if workRecordEnforceEnabled() {
			t.Errorf("workRecordEnforceEnabled(%q) = true, want false", v)
		}
	}
}
