package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// stampMetaSpy captures the (beadID, assignee, patch) a claim writes through the
// StampWorkMeta seam, and lets a test inject a write error to prove the stamp
// never fails the claim. The patch is copied so the assertion is stable.
type stampMetaSpy struct {
	calls    int
	beadID   string
	assignee string
	patch    map[string]string
	err      error
}

func (s *stampMetaSpy) fn(_ context.Context, _ string, _ []string, beadID, assignee string, patch map[string]string) error {
	s.calls++
	s.beadID, s.assignee = beadID, assignee
	s.patch = map[string]string{}
	for k, v := range patch {
		s.patch[k] = v
	}
	return s.err
}

// noopPublishRunMap keeps claim tests focused on work-bead identity stamping.
func noopPublishRunMap(string, string, ...string) error {
	return nil
}

// noopStampWorkMeta suppresses the work-bead identity stamp so claim tests that
// don't assert on it stay hermetic — the default seam issues a real bd subprocess
// write, which fires now that a claim stamps session identity whenever GC_SESSION_ID
// is set.
func noopStampWorkMeta(context.Context, string, []string, string, string, map[string]string) error {
	return nil
}

// poolClaimOps builds the seam for a pool slot claiming an unassigned,
// route-matched candidate: the runner yields it, Claim returns it owned by us,
// the branch resolver returns branch, and StampWorkMeta is captured by spy.
func poolClaimOps(runner string, claimedMeta map[string]string, branch string, spy *stampMetaSpy) hookClaimOps {
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return runner, nil },
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: claimedMeta}, true, nil
		},
		ResolveWorkBranch: func(string) string { return branch },
		StampWorkMeta:     spy.fn,
		PublishRunMap:     noopPublishRunMap,
	}
}

// poolClaimOpts is a pool slot's claim options: assignee is the pool session name
// and the env carries both GC_SESSION_ID (the session bead id) and
// GC_SESSION_NAME (the pool session name).
func poolClaimOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "gc__role-mc-sess1",
		IdentityCandidates: []string{"gc__role-mc-sess1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_SESSION_ID=mc-sess1", "GC_SESSION_NAME=gc__role-mc-sess1"},
		JSON:               true,
	}
}

// TestDoHookClaimStampsSessionIdentity is the primary claim-time back-reference
// test: a fresh pool claim stamps gc.session_id + gc.session_name onto the work
// bead alongside gc.work_branch, in ONE patch. Fails before the fix, which stamped
// only the branch.
func TestDoHookClaimStampsSessionIdentity(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hw-pool","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"bd-hw-pool",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 1 {
		t.Fatalf("StampWorkMeta calls = %d, want 1", spy.calls)
	}
	want := map[string]string{
		beadmeta.WorkBranchMetadataKey:  "bd-hw-pool",
		beadmeta.SessionIDMetadataKey:   "mc-sess1",
		beadmeta.SessionNameMetadataKey: "gc__role-mc-sess1",
	}
	if !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("patch = %v, want %v", spy.patch, want)
	}
	if spy.beadID != "hw-pool" || spy.assignee != "gc__role-mc-sess1" {
		t.Fatalf("stamp target = bead %q assignee %q, want hw-pool / gc__role-mc-sess1", spy.beadID, spy.assignee)
	}
}

// TestDoHookClaimStampsSessionIdentityOnAdoption covers the adoption path: a bead
// already in_progress and owned by this session (existing_assignment, no fresh
// Claim) still receives the session back-reference, since the stamp re-runs on
// every hook tick that adopts the bead.
func TestDoHookClaimStampsSessionIdentityOnAdoption(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"hw-adopt","status":"in_progress","assignee":"gc__role-mc-sess1","metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			t.Error("Claim must not be called on the existing-assignment path")
			return beads.Bead{}, false, nil
		},
		ResolveWorkBranch: func(string) string { return "" }, // no worktree
		StampWorkMeta:     spy.fn,
		PublishRunMap:     noopPublishRunMap,
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{
		beadmeta.SessionIDMetadataKey:   "mc-sess1",
		beadmeta.SessionNameMetadataKey: "gc__role-mc-sess1",
	}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v}", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimStampsSessionIdentityWithoutWorktree pins sessionVerify #1: when
// the worktree resolves no branch (no repo / detached HEAD), the session
// back-reference is STILL stamped — it must not be buried behind the branch
// early-return.
func TestDoHookClaimStampsSessionIdentityWithoutWorktree(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hw-nobranch","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"", // no branch
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{
		beadmeta.SessionIDMetadataKey:   "mc-sess1",
		beadmeta.SessionNameMetadataKey: "gc__role-mc-sess1",
	}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v} (session id/name even with no worktree)", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimSkipsStampWhenIdentityUnchanged pins the mandatory idempotence
// guard (sessionVerify #2): a candidate already carrying the current branch AND
// session identity produces NO write, so the per-tick adoption re-run does not
// flood bead.updated events.
func TestDoHookClaimSkipsStampWhenIdentityUnchanged(t *testing.T) {
	spy := &stampMetaSpy{}
	current := map[string]string{
		"gc.routed_to":    "worker",
		"gc.work_branch":  "bd-hw-idem",
		"gc.session_id":   "mc-sess1",
		"gc.session_name": "gc__role-mc-sess1",
	}
	ops := poolClaimOps(
		`[{"id":"hw-idem","status":"open","metadata":{"gc.routed_to":"worker","gc.work_branch":"bd-hw-idem","gc.session_id":"mc-sess1","gc.session_name":"gc__role-mc-sess1"}}]`,
		current,
		"bd-hw-idem",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 0 {
		t.Fatalf("StampWorkMeta calls = %d, want 0 (branch + session identity already current)", spy.calls)
	}
}

// TestDoHookClaimStampsOnlyChangedIdentityKeys proves the patch is minimal: a
// candidate whose session identity is current but whose branch changed writes ONLY
// the branch, leaving the unchanged session keys out of the patch.
func TestDoHookClaimStampsOnlyChangedIdentityKeys(t *testing.T) {
	spy := &stampMetaSpy{}
	current := map[string]string{
		"gc.routed_to":    "worker",
		"gc.work_branch":  "bd-old",
		"gc.session_id":   "mc-sess1",
		"gc.session_name": "gc__role-mc-sess1",
	}
	ops := poolClaimOps(
		`[{"id":"hw-partial","status":"open","metadata":{"gc.routed_to":"worker","gc.work_branch":"bd-old","gc.session_id":"mc-sess1","gc.session_name":"gc__role-mc-sess1"}}]`,
		current,
		"bd-new",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{beadmeta.WorkBranchMetadataKey: "bd-new"}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v} (only the changed key)", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimSkipsSessionIdentityForControlBead pins the control-bead edge
// policy (sessionVerify #3): a control-dispatcher session claiming a control bead
// (gc.kind in ControlKinds) must NOT acquire a session back-reference — control
// steps stay session-free by graphroute's design — while gc.work_branch is still
// stamped as before.
func TestDoHookClaimSkipsSessionIdentityForControlBead(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hc-check","status":"open","metadata":{"gc.routed_to":"worker","gc.kind":"check"}}]`,
		map[string]string{"gc.routed_to": "worker", "gc.kind": "check"},
		"bd-hc-check",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{beadmeta.WorkBranchMetadataKey: "bd-hc-check"}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v} (no session keys on a control bead)", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimSkipsSessionIdentityWhenNoSessionID: a non-session run (no
// GC_SESSION_ID) has no session bead to reference, so neither session key is
// stamped even when GC_SESSION_NAME happens to be set.
func TestDoHookClaimSkipsSessionIdentityWhenNoSessionID(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hw-nosess","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"bd-hw-nosess",
		spy,
	)
	opts := poolClaimOpts()
	opts.Env = []string{"GC_SESSION_NAME=gc__role-mc-sess1"} // GC_SESSION_ID absent

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{beadmeta.WorkBranchMetadataKey: "bd-hw-nosess"}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v} (no session id ⇒ no session keys)", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimIdentityStampFailureDoesNotFailClaim proves the stamp is
// best-effort: a failing StampWorkMeta logs to stderr but the claim still exits 0
// and reports the claimed bead id.
func TestDoHookClaimIdentityStampFailureDoesNotFailClaim(t *testing.T) {
	spy := &stampMetaSpy{err: errors.New("dolt boom")}
	ops := poolClaimOps(
		`[{"id":"hw-err","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"bd-hw-err",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stamp error must not fail the claim); stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.BeadID != "hw-err" || result.Reason != "claimed" {
		t.Fatalf("claim result = %+v, want bead hw-err reason claimed", result)
	}
}

// workerProvenanceClaimOpts is poolClaimOpts plus a resolved worker
// provenance triple, the shape cmdHookWithOptions hands the claim path for a
// live session.
func workerProvenanceClaimOpts() hookClaimOptions {
	opts := poolClaimOpts()
	opts.Worker = workerProvenance{Model: "fable-5", Genus: "anthropic", Effort: "low"}
	return opts
}

// TestDoHookClaimStampsWorkerProvenance is the primary cross-genus provenance
// test (ga-vkcp): a fresh pool claim stamps worker_model / worker_genus /
// worker_effort onto the work bead in the SAME single patch as the branch and
// session back-reference, so every dispatch route records executor lineage.
func TestDoHookClaimStampsWorkerProvenance(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hw-prov","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"bd-hw-prov",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", workerProvenanceClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{
		beadmeta.WorkBranchMetadataKey:   "bd-hw-prov",
		beadmeta.SessionIDMetadataKey:    "mc-sess1",
		beadmeta.SessionNameMetadataKey:  "gc__role-mc-sess1",
		beadmeta.WorkerModelMetadataKey:  "fable-5",
		beadmeta.WorkerGenusMetadataKey:  "anthropic",
		beadmeta.WorkerEffortMetadataKey: "low",
	}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v}", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimSkipsWorkerProvenanceWhenCurrent extends the idempotence
// guard: a bead already carrying the current provenance triple (plus branch
// and session identity) produces NO write on the per-tick adoption re-run.
func TestDoHookClaimSkipsWorkerProvenanceWhenCurrent(t *testing.T) {
	spy := &stampMetaSpy{}
	current := `{"gc.routed_to":"worker","gc.work_branch":"bd-hw-pidem","gc.session_id":"mc-sess1","gc.session_name":"gc__role-mc-sess1","worker_model":"fable-5","worker_genus":"anthropic","worker_effort":"low"}`
	ops := poolClaimOps(
		`[{"id":"hw-pidem","status":"open","metadata":`+current+`}]`,
		map[string]string{
			"gc.routed_to": "worker", "gc.work_branch": "bd-hw-pidem",
			"gc.session_id": "mc-sess1", "gc.session_name": "gc__role-mc-sess1",
			"worker_model": "fable-5", "worker_genus": "anthropic", "worker_effort": "low",
		},
		"bd-hw-pidem",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", workerProvenanceClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 0 {
		t.Fatalf("StampWorkMeta calls = %d, want 0 (provenance already current)", spy.calls)
	}
}

// TestDoHookClaimStampsPartialWorkerProvenance: an unresolvable model (the
// provider CLI picks its own default) must not block the genus/effort stamp,
// and no fabricated worker_model key may appear.
func TestDoHookClaimStampsPartialWorkerProvenance(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hw-partialprov","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"",
		spy,
	)
	opts := poolClaimOpts()
	opts.Worker = workerProvenance{Genus: "anthropic", Effort: "max"}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{
		beadmeta.SessionIDMetadataKey:    "mc-sess1",
		beadmeta.SessionNameMetadataKey:  "gc__role-mc-sess1",
		beadmeta.WorkerGenusMetadataKey:  "anthropic",
		beadmeta.WorkerEffortMetadataKey: "max",
	}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v} (no fabricated worker_model)", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimSkipsWorkerProvenanceForControlBead: control beads stay
// session-free AND provenance-free — the control dispatcher executing a check
// is infrastructure, not a worker whose lineage the cross-genus gate reviews.
func TestDoHookClaimSkipsWorkerProvenanceForControlBead(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hc-provcheck","status":"open","metadata":{"gc.routed_to":"worker","gc.kind":"check"}}]`,
		map[string]string{"gc.routed_to": "worker", "gc.kind": "check"},
		"bd-hc-provcheck",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", workerProvenanceClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{beadmeta.WorkBranchMetadataKey: "bd-hc-provcheck"}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v} (no provenance on a control bead)", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimSkipsWorkerProvenanceWhenNoSessionID: outside a live session
// (no GC_SESSION_ID) the executor is unknown to gc, so no provenance is
// stamped even when the template's config would resolve one.
func TestDoHookClaimSkipsWorkerProvenanceWhenNoSessionID(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hw-provnosess","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"bd-hw-provnosess",
		spy,
	)
	opts := workerProvenanceClaimOpts()
	opts.Env = []string{"GC_SESSION_NAME=gc__role-mc-sess1"} // GC_SESSION_ID absent

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{beadmeta.WorkBranchMetadataKey: "bd-hw-provnosess"}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v} (no session ⇒ no provenance)", spy.calls, spy.patch, want)
	}
}
