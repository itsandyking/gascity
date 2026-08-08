package bootstrap

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

func compileCorePolecatCommit(t *testing.T) *formula.Recipe {
	t.Helper()
	prev := formula.IsFormulaV2Enabled()
	formula.SetFormulaV2Enabled(true)
	t.Cleanup(func() { formula.SetFormulaV2Enabled(prev) })

	recipe, err := formula.Compile(context.Background(), "mol-polecat-commit", coreFormulaSearchPaths(t), map[string]string{
		"convoy_id":   "gc-convoy",
		"base_branch": "main",
	})
	if err != nil {
		t.Fatalf("compile mol-polecat-commit: %v", err)
	}
	return recipe
}

// The push gate must be off by default: mol-polecat-commit is the
// direct-commit variant for installations without merge review, and rigs
// that require review opt in via formula_vars (podcast-updates-zf22).
func TestCoreMolPolecatCommitDeclaresRequireReviewVerdictVar(t *testing.T) {
	recipe := compileCorePolecatCommit(t)

	def, ok := recipe.Vars["require_review_verdict"]
	if !ok {
		t.Fatal("recipe missing require_review_verdict var")
	}
	if def.Default == nil {
		t.Fatal("require_review_verdict must have a default so the formula compiles without rig configuration")
	}
	if *def.Default != "false" {
		t.Fatalf("require_review_verdict default = %q, want %q", *def.Default, "false")
	}
}

// The commit-and-push step must evaluate a mechanical push gate before any
// push: an unconditional hold check on the work bead, plus (when
// require_review_verdict is set) a durable review verdict that covers the
// exact HEAD being pushed. Four incidents on 2026-08-08 (podcast-updates-cde,
// -u4r, -c0z, -l5de) came from the push directive firing on beads whose
// contract forbade landing; c0z pushed unreviewed work to origin/main.
func TestCoreMolPolecatCommitPushStepGatesPushOnReviewVerdict(t *testing.T) {
	recipe := compileCorePolecatCommit(t)
	step := recipe.StepByID("mol-polecat-commit.commit-and-push")
	if step == nil {
		t.Fatal("recipe missing mol-polecat-commit.commit-and-push step")
	}
	description := step.Description

	for _, want := range []string{
		"{{require_review_verdict}}",
		"gc.review_verdict",
		"gc.reviewed_commit",
		`select(startswith("hold:"))`,
		"PUSH_BLOCKED",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("commit-and-push description missing %q:\n%s", want, description)
		}
	}

	// The gate must be evaluated before the push loop, not after.
	gateAt := strings.Index(description, "PUSH_BLOCKED")
	pushAt := strings.Index(description, "git push origin")
	if pushAt < 0 {
		t.Fatalf("commit-and-push description missing the push command:\n%s", description)
	}
	if gateAt > pushAt {
		t.Fatalf("push gate (index %d) must be evaluated before the push (index %d)", gateAt, pushAt)
	}

	// A stale verdict must not ride: the gate compares the reviewed commit
	// against the HEAD actually being pushed.
	if !strings.Contains(description, `"$REVIEWED_COMMIT" != "$HEAD_COMMIT"`) {
		t.Fatalf("commit-and-push gate must require the review verdict to cover HEAD:\n%s", description)
	}
}

// Every non-push exit from commit-and-push must anchor the halt commit at
// work/<bead-id> first: a detached-HEAD worktree with zero refs orphans its
// commits (three needed manual mayor anchoring on 2026-08-08). Both the
// gate-closed halt and the push-retry-exhausted exit must anchor.
func TestCoreMolPolecatCommitHaltAnchorsWorkRef(t *testing.T) {
	recipe := compileCorePolecatCommit(t)
	step := recipe.StepByID("mol-polecat-commit.commit-and-push")
	if step == nil {
		t.Fatal("recipe missing mol-polecat-commit.commit-and-push step")
	}
	description := step.Description

	const anchor = `git branch -f "work/$WORK_BEAD_ID"`
	if got := strings.Count(description, anchor); got < 2 {
		t.Fatalf("commit-and-push must anchor work/<bead> on every non-push exit (gate-closed halt AND push-retry exhaustion); found %d occurrence(s) of %q:\n%s", got, anchor, description)
	}

	// The halt must leave a durable pointer to the anchored work on the bead.
	for _, want := range []string{
		"gc.work_commit",
		"gc.work_branch",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("commit-and-push halt must stamp %q on the work bead:\n%s", want, description)
		}
	}

	// The halt parks this step so it stops re-firing until an actor opens the
	// gate — the manual-parking race is what let c0z fire.
	if !strings.Contains(description, "hold=mayor") {
		t.Fatalf("commit-and-push halt must park the step with hold=mayor:\n%s", description)
	}
}

// workspace-setup must never adopt a checkout it did not create: it reuses
// only a worktree recorded on the bead that passes ownership validation, and
// otherwise always runs git worktree add. A polecat committing inside the
// mayor's landing worktree (/private/tmp/pu-land, 2026-08-08) is the incident
// this guards against.
func TestCoreMolPolecatCommitWorkspaceSetupRefusesForeignCheckouts(t *testing.T) {
	recipe := compileCorePolecatCommit(t)
	step := recipe.StepByID("mol-polecat-commit.workspace-setup")
	if step == nil {
		t.Fatal("recipe missing mol-polecat-commit.workspace-setup step")
	}
	description := step.Description

	for _, want := range []string{
		"git worktree add",
		"gc.work_dir",
		"--git-common-dir",
		`case "$(basename`,
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("workspace-setup description missing %q:\n%s", want, description)
		}
	}

	// The old blind-adopt path entered whatever path metadata named and
	// pulled: reuse now happens only behind ownership validation.
	if strings.Contains(description, "git pull --rebase") {
		t.Fatalf("workspace-setup must not blind-adopt a recorded path with git pull --rebase:\n%s", description)
	}
}

// The commit step re-proves worktree ownership before committing anything:
// the straggler auto-commit (git add -A) must not run in a checkout the
// formula does not own (f02f218 landed on the mayor's landing branch that
// way).
func TestCoreMolPolecatCommitPushStepReprovesOwnershipBeforeCommitting(t *testing.T) {
	recipe := compileCorePolecatCommit(t)
	step := recipe.StepByID("mol-polecat-commit.commit-and-push")
	if step == nil {
		t.Fatal("recipe missing mol-polecat-commit.commit-and-push step")
	}
	description := step.Description

	guardAt := strings.Index(description, "--git-common-dir")
	commitAt := strings.Index(description, "git add -A")
	if guardAt < 0 {
		t.Fatalf("commit-and-push missing the worktree ownership guard:\n%s", description)
	}
	if commitAt < 0 {
		t.Fatalf("commit-and-push missing the straggler capture commit:\n%s", description)
	}
	if guardAt > commitAt {
		t.Fatalf("ownership guard (index %d) must run before the straggler commit (index %d)", guardAt, commitAt)
	}
}

// The pushed path closes the work bead with the typed work record (ADR-0009)
// so the close gate can verify the landing instead of trusting prose.
func TestCoreMolPolecatCommitPushStepStampsWorkRecordOnClose(t *testing.T) {
	recipe := compileCorePolecatCommit(t)
	step := recipe.StepByID("mol-polecat-commit.commit-and-push")
	if step == nil {
		t.Fatal("recipe missing mol-polecat-commit.commit-and-push step")
	}
	description := step.Description

	for _, want := range []string{
		"gc.work_outcome=shipped",
		"gc bd close",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("commit-and-push close must include %q:\n%s", want, description)
		}
	}
}
