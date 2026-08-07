package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Work-record close gate (ADR-0009). Closing a work bead through the SDK close
// seam (`gc bd close`) is validated against the typed work-record contract: the
// bead must carry a typed gc.work_outcome, and a "shipped" outcome must point at
// a commit that is reachable on the stamped gc.work_branch. This turns the
// recurring "drain-without-commit" close (a close that leaves no artifact at
// all) into a machine-checkable violation.
//
// The gate ships warn-only by default — violations are logged but the close
// proceeds — so existing open beads migrate without breakage. Set
// GC_WORK_RECORD_ENFORCE to a truthy value to make violations block the close.
//
// The pass-close contract (ga-dnay) rides the same seam but is ALWAYS
// enforced, independent of GC_WORK_RECORD_ENFORCE: a close carrying the
// control-plane gc.outcome=pass on a worker-claimable bead must name a
// gc.work_commit that exists and is reachable from a branch, with the working
// tree clean of non-infrastructure changes (.agents/.codex/.gc excluded).
// Twice on 2026-08-07 a worker closed "pass" with its entire diff uncommitted
// on a detached HEAD — work that existed nowhere in git history and died with
// the checkout. "pass" must mean the work landed.

// workRecordEnforceEnvVar gates whether work-record violations block the close
// (enforce) or are logged only (warn-only, the default).
const workRecordEnforceEnvVar = "GC_WORK_RECORD_ENFORCE"

// workRecordEnforceEnabled reports whether the close gate should block closes
// that violate the work-record contract, rather than only warning.
func workRecordEnforceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(workRecordEnforceEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// validWorkOutcome reports whether v is one of the four typed work-record close
// dispositions. The vocabulary is owned here (the consumer), not in beadmeta,
// per that package's data-only convention.
func validWorkOutcome(v string) bool {
	switch v {
	case beadmeta.WorkOutcomeShipped, beadmeta.WorkOutcomeNoOp,
		beadmeta.WorkOutcomeBlocked, beadmeta.WorkOutcomeAbandoned:
		return true
	default:
		return false
	}
}

// isWorkRecordGatedBead reports whether the work-record close contract applies
// to bead. It applies to worker-claimable work units — plain task beads — and
// deliberately NOT to control/structural beads (anything carrying gc.kind:
// workflow roots, scope/run/check/drain steps, etc.) or non-task beads (convoy,
// message). Those use the disjoint control-plane gc.outcome vocabulary and are
// closed by the dispatch engine, not by a worker reporting a work outcome.
func isWorkRecordGatedBead(bead beads.Bead) bool {
	if t := strings.TrimSpace(bead.Type); t != "" && t != "task" {
		return false
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey]) != "" {
		return false
	}
	return true
}

// validateWorkRecordOnClose checks bead against the typed work-record contract
// and returns a human-readable message for each violation (empty slice ⇒ the
// bead satisfies the contract). commitReachable reports whether a commit SHA is
// an ancestor of a branch; it is injected so the rule is unit-testable without
// a real repo. The caller is responsible for scoping (isWorkRecordGatedBead).
func validateWorkRecordOnClose(bead beads.Bead, commitReachable func(commit, branch string) bool) []string {
	outcome := strings.TrimSpace(bead.Metadata[beadmeta.WorkOutcomeMetadataKey])
	if outcome == "" {
		return []string{fmt.Sprintf("missing %s (want one of shipped|no-op|blocked|abandoned)", beadmeta.WorkOutcomeMetadataKey)}
	}
	if !validWorkOutcome(outcome) {
		return []string{fmt.Sprintf("invalid %s=%q (want one of shipped|no-op|blocked|abandoned)", beadmeta.WorkOutcomeMetadataKey, outcome)}
	}
	if outcome != beadmeta.WorkOutcomeShipped {
		// no-op / blocked / abandoned carry their reason in the close-reason; no
		// commit artifact is required.
		return nil
	}
	commit := strings.TrimSpace(bead.Metadata[beadmeta.WorkCommitMetadataKey])
	branch := strings.TrimSpace(bead.Metadata[beadmeta.WorkBranchMetadataKey])
	var violations []string
	if commit == "" {
		violations = append(violations, fmt.Sprintf("%s=shipped requires %s (the commit that satisfied the bead)", beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkCommitMetadataKey))
	}
	if branch == "" {
		violations = append(violations, fmt.Sprintf("%s=shipped requires %s (the branch the commit lives on)", beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkBranchMetadataKey))
	}
	if commit != "" && branch != "" && !commitReachable(commit, branch) {
		violations = append(violations, fmt.Sprintf("%s %s is not reachable on %s %s", beadmeta.WorkCommitMetadataKey, commit, beadmeta.WorkBranchMetadataKey, branch))
	}
	return violations
}

// passCloseChecks bundles the repo-state predicates the pass-close gate needs.
// They are injected so the validation rule is unit-testable without a real git
// repository; evaluateWorkRecordCloseGate binds them to git commands run in the
// bead's work directory.
type passCloseChecks struct {
	// commitExists reports whether the SHA names a commit object in the repo.
	commitExists func(commit string) bool
	// commitOnAnyBranch reports whether the commit is reachable from at least
	// one local or remote-tracking branch (a detached-HEAD-only commit is not).
	commitOnAnyBranch func(commit string) bool
	// dirtyPaths returns every changed or untracked path in the working tree
	// (unfiltered; the validation applies the infrastructure exclusions).
	dirtyPaths func() ([]string, error)
}

// passCloseInfraDirs are the top-level session-infrastructure directories whose
// dirt never blocks a pass close: they are runtime state materialized into a
// worker's checkout, not work product.
var passCloseInfraDirs = []string{".agents", ".codex", ".gc"}

// passCloseDirtyPathsShown caps how many dirty paths a violation message names.
const passCloseDirtyPathsShown = 5

// validatePassCloseOnClose checks a close carrying the control-plane
// gc.outcome=pass against the landed-work contract and returns a
// human-readable message per violation (empty ⇒ the close may proceed). The
// contract: gc.work_commit must name a commit that exists and is reachable
// from a branch, and the working tree must be clean of non-infrastructure
// changes. Closes with any other gc.outcome (or none) are exempt — fail,
// skipped, and canceled carry no landing. The caller is responsible for
// scoping (isWorkRecordGatedBead).
func validatePassCloseOnClose(bead beads.Bead, checks passCloseChecks) []string {
	if strings.TrimSpace(bead.Metadata[beadmeta.OutcomeMetadataKey]) != beadmeta.OutcomePass {
		return nil
	}
	var violations []string
	commit := strings.TrimSpace(bead.Metadata[beadmeta.WorkCommitMetadataKey])
	switch {
	case commit == "":
		violations = append(violations, fmt.Sprintf("%s=%s requires %s (the commit that landed this bead's work)", beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass, beadmeta.WorkCommitMetadataKey))
	case !checks.commitExists(commit):
		violations = append(violations, fmt.Sprintf("%s %q names a commit that does not exist in the work repository", beadmeta.WorkCommitMetadataKey, commit))
	case !checks.commitOnAnyBranch(commit):
		violations = append(violations, fmt.Sprintf("%s %q is not reachable from any branch — a commit only on a detached HEAD dies with the checkout", beadmeta.WorkCommitMetadataKey, commit))
	}
	dirty, err := checks.dirtyPaths()
	if err != nil {
		violations = append(violations, fmt.Sprintf("cannot verify the working tree is clean: %v", err))
	} else if nonInfra := filterPassCloseDirtyPaths(dirty); len(nonInfra) > 0 {
		violations = append(violations, fmt.Sprintf("the working tree has uncommitted non-infrastructure changes (%s)", summarizePassCloseDirtyPaths(nonInfra)))
	}
	return violations
}

// isPassCloseInfraPath reports whether a worktree-root-relative path lives in
// one of the session-infrastructure directories excluded from the pass-close
// clean-tree check. Only top-level infra directories match: a same-named
// directory nested deeper in the project is tracked content, not runtime state.
func isPassCloseInfraPath(path string) bool {
	path = strings.TrimPrefix(strings.TrimSpace(path), "./")
	for _, dir := range passCloseInfraDirs {
		if path == dir || strings.HasPrefix(path, dir+"/") {
			return true
		}
	}
	return false
}

// filterPassCloseDirtyPaths drops infrastructure paths (and blanks) from a
// dirty-path list, leaving only the entries that count as uncommitted work.
func filterPassCloseDirtyPaths(paths []string) []string {
	var nonInfra []string
	for _, p := range paths {
		if strings.TrimSpace(p) == "" || isPassCloseInfraPath(p) {
			continue
		}
		nonInfra = append(nonInfra, p)
	}
	return nonInfra
}

// summarizePassCloseDirtyPaths renders a dirty-path list for a violation
// message, capping how many are named so a large diff doesn't flood stderr.
func summarizePassCloseDirtyPaths(paths []string) string {
	if len(paths) <= passCloseDirtyPathsShown {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:passCloseDirtyPathsShown], ", "), len(paths)-passCloseDirtyPathsShown)
}

// gitCommitExists reports whether commit names an existing commit object in
// the repository at repoDir. A flag-shaped value (leading "-") is rejected
// outright so malformed metadata can never be parsed as a git option.
func gitCommitExists(repoDir, commit string) bool {
	if strings.TrimSpace(repoDir) == "" || commit == "" || strings.HasPrefix(commit, "-") {
		return false
	}
	return exec.Command("git", "-C", repoDir, "cat-file", "-e", commit+"^{commit}").Run() == nil
}

// gitCommitReachableFromAnyBranch reports whether commit is reachable from at
// least one local or remote-tracking branch in the repository at repoDir. A
// commit reachable only from a detached HEAD (or no ref at all) reports false:
// it does not survive the checkout being discarded. Git errors read as "not
// reachable", and flag-shaped values are rejected as in gitCommitExists.
func gitCommitReachableFromAnyBranch(repoDir, commit string) bool {
	if strings.TrimSpace(repoDir) == "" || commit == "" || strings.HasPrefix(commit, "-") {
		return false
	}
	out, err := exec.Command("git", "-C", repoDir, "for-each-ref", "--contains", commit, "--format=%(refname)", "refs/heads", "refs/remotes").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// gitDirtyWorkTreePaths returns every changed or untracked path reported by
// `git status --porcelain -z` in repoDir, including both sides of a rename.
// Ignored files are excluded by porcelain itself, so a path the repository has
// deliberately gitignored never counts as dirt. Errors (including repoDir not
// being a git repository) surface to the caller, which fails closed.
func gitDirtyWorkTreePaths(repoDir string) ([]string, error) {
	if strings.TrimSpace(repoDir) == "" {
		return nil, fmt.Errorf("no work directory to inspect")
	}
	out, err := exec.Command("git", "-C", repoDir, "status", "--porcelain", "-z").Output()
	if err != nil {
		// Surface git's own first stderr line (e.g. "fatal: not a git
		// repository") — a bare exit status gives a refused worker nothing
		// actionable.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if line := strings.TrimSpace(strings.SplitN(string(exitErr.Stderr), "\n", 2)[0]); line != "" {
				return nil, fmt.Errorf("git status in %s: %w: %s", repoDir, err, line)
			}
		}
		return nil, fmt.Errorf("git status in %s: %w", repoDir, err)
	}
	records := strings.Split(string(out), "\x00")
	var paths []string
	for i := 0; i < len(records); i++ {
		record := records[i]
		// "XY <path>": two status columns, a space, then the path.
		if len(record) < 4 {
			continue
		}
		paths = append(paths, record[3:])
		// A rename/copy in either column carries its source path as the next
		// NUL-separated record.
		if record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C' {
			i++
			if i < len(records) && records[i] != "" {
				paths = append(paths, records[i])
			}
		}
	}
	return paths, nil
}

// gitCommitReachableOnBranch reports whether commit is an ancestor of branch in
// the git repository at repoDir (worktrees share one object store, so any
// worktree dir resolves refs across the repo). A non-nil error from git — bad
// repo, unknown ref, unknown commit — reads as "not reachable". A commit/branch
// that looks like a flag (leading "-") is rejected outright so a malformed
// metadata value can never be parsed as a git option.
func gitCommitReachableOnBranch(repoDir, commit, branch string) bool {
	if strings.TrimSpace(repoDir) == "" || commit == "" || branch == "" {
		return false
	}
	if strings.HasPrefix(commit, "-") || strings.HasPrefix(branch, "-") {
		return false
	}
	return exec.Command("git", "-C", repoDir, "merge-base", "--is-ancestor", commit, branch).Run() == nil
}

// workRecordCloseTargets returns the bead IDs a bd invocation closes, and
// whether the invocation is a close at all. It covers both forms the SDK seam
// sees: the `close` subcommand and `update --status=closed` (the form the
// worker formulas use to stamp metadata and close in one call). Ambiguous or
// ID-less invocations report not-a-close so the gate stays out of the way.
func workRecordCloseTargets(bdArgs []string) ([]string, bool) {
	if len(bdArgs) == 0 {
		return nil, false
	}
	switch bdArgs[0] {
	case "close":
	case "update":
		if !bdUpdateClosesStatus(bdArgs) {
			return nil, false
		}
	default:
		return nil, false
	}
	ids, ok, ambiguous := bdMutationWriteIDs(bdArgs)
	if !ok || ambiguous || len(ids) == 0 {
		return nil, false
	}
	return ids, true
}

// bdUpdateClosesStatus reports whether a `bd update` arg list sets the status to
// "closed" (in any of the --status=closed, --status closed, -s closed forms).
// bd registers status as a scalar flag, so the last occurrence wins. Values of
// other known flags are consumed before looking for status, and `--` terminates
// flag parsing, matching the mutation target scanner and pflag.
func bdUpdateClosesStatus(bdArgs []string) bool {
	valueFlags := bdSubcmdValueFlags("update")
	status := ""
	seen := false
	for i := 1; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		if arg == "--" {
			break
		}
		if v, ok := strings.CutPrefix(arg, "--status="); ok {
			status, seen = v, true
			continue
		}
		if v, ok := strings.CutPrefix(arg, "-s="); ok {
			status, seen = v, true
			continue
		}
		if arg == "--status" || arg == "-s" {
			if i+1 >= len(bdArgs) {
				return false
			}
			i++
			status, seen = bdArgs[i], true
			continue
		}
		if !strings.Contains(arg, "=") && valueFlags[arg] && i+1 < len(bdArgs) {
			i++
		}
	}
	return seen && strings.EqualFold(strings.TrimSpace(status), "closed")
}

// runWorkRecordCloseGate validates every bead a `gc bd close` (or
// `gc bd update --status=closed`) invocation closes against the work-record
// contract and the pass-close contract. Best-effort: it never blocks on its
// own read failure. Returns whether the close should be blocked — work-record
// violations block only when enforcement is enabled; pass-close violations
// always block.
//
// preOpened and preFetched let a caller that already opened the store and
// fetched the target beads (e.g. the write-ID collision guard, which reads
// the same beads for the same IDs immediately before this gate runs) hand
// them in instead of paying a second openStoreAtForCity + store.Get round
// trip. Both are optional (nil is fine): preOpened falls back to opening its
// own store, and any ID missing from preFetched falls back to store.Get.
func runWorkRecordCloseGate(bdArgs []string, scopeRoot, cityPath string, cfg *config.City, preOpened beads.Store, preFetched map[string]beads.Bead, stderr io.Writer) bool {
	if _, ok := workRecordCloseTargets(bdArgs); !ok {
		return false
	}
	store := preOpened
	if store == nil {
		var err error
		store, err = openStoreAtForCityWithConfig(scopeRoot, cityPath, cfg)
		if err != nil {
			// Cannot verify — never block a close on our own read failure.
			return false
		}
	}
	return evaluateWorkRecordCloseGate(bdArgs, store, preFetched, scopeRoot, workRecordEnforceEnabled(), stderr)
}

// evaluateWorkRecordCloseGate is the store-driven core of the close gate, split
// from the IO wrapper so it is unit-testable with an in-memory store. It logs
// each violation and reports whether the close should be blocked. preFetched
// (optional) supplies beads already read by an earlier guard in this same
// invocation, avoiding a duplicate store.Get for the same ID.
func evaluateWorkRecordCloseGate(bdArgs []string, store beads.Store, preFetched map[string]beads.Bead, scopeRoot string, enforce bool, stderr io.Writer) (block bool) {
	ids, ok := workRecordCloseTargets(bdArgs)
	if !ok {
		return false
	}
	mode := "warn-only"
	if enforce {
		mode = "enforced"
	}
	for _, id := range ids {
		bead, cached := preFetched[id]
		if !cached {
			var getErr error
			bead, getErr = store.Get(id)
			if getErr != nil {
				continue
			}
		}
		if !isWorkRecordGatedBead(bead) {
			continue
		}
		var projectionErr error
		bead, projectionErr = applyWorkRecordUpdateMetadata(bead, bdArgs)
		repoDir := strings.TrimSpace(bead.Metadata[beadmeta.WorkDirMetadataKey])
		if repoDir == "" {
			repoDir = scopeRoot
		}
		var violations, passViolations []string
		if projectionErr != nil {
			violations = []string{projectionErr.Error()}
		} else {
			violations = validateWorkRecordOnClose(bead, func(commit, branch string) bool {
				return gitCommitReachableOnBranch(repoDir, commit, branch)
			})
			passViolations = validatePassCloseOnClose(bead, passCloseChecks{
				commitExists:      func(commit string) bool { return gitCommitExists(repoDir, commit) },
				commitOnAnyBranch: func(commit string) bool { return gitCommitReachableFromAnyBranch(repoDir, commit) },
				dirtyPaths:        func() ([]string, error) { return gitDirtyWorkTreePaths(repoDir) },
			})
		}
		for _, v := range violations {
			fmt.Fprintf(stderr, "gc bd: work-record gate (%s): close of %s: %s\n", mode, id, v) //nolint:errcheck // best-effort stderr
		}
		if enforce && len(violations) > 0 {
			block = true
		}
		for _, v := range passViolations {
			fmt.Fprintf(stderr, "gc bd: pass-close gate: close of %s: %s\n", id, v) //nolint:errcheck // best-effort stderr
		}
		if len(passViolations) > 0 {
			branchHint := "the work branch"
			if branch := strings.TrimSpace(bead.Metadata[beadmeta.WorkBranchMetadataKey]); branch != "" {
				branchHint = fmt.Sprintf("work branch %q", branch)
			}
			fmt.Fprintf(stderr, "gc bd: pass-close gate: refusing close of %s — commit the work to %s first, stamp %s with the landed commit, and retry the close\n", id, branchHint, beadmeta.WorkCommitMetadataKey) //nolint:errcheck // best-effort stderr
			block = true
		}
	}
	return block
}

// workRecordMetadataEdits is the parsed metadata mutation of a `bd update` arg
// list: either a whole-object --metadata merge (hasMetadataJSON) or a set of
// --set-metadata / --unset-metadata edits. The two forms are mutually exclusive
// in bd; applyWorkRecordMetadataEdits enforces that.
type workRecordMetadataEdits struct {
	metadataJSON    string
	hasMetadataJSON bool
	setMetadata     []string
	unsetMetadata   []string
}

// applyWorkRecordUpdateMetadata overlays metadata mutations from an atomic
// `bd update ... --status=closed` invocation onto the stored bead before the
// close gate validates it. The documented worker close form stamps the typed
// work record and closes in one update, so validating only the pre-update bead
// would reject a valid enforced close and warn incorrectly in migration mode.
//
// The parse and apply phases are split so neither carries the whole projection's
// branch density; together they match bd's update flag semantics exactly.
func applyWorkRecordUpdateMetadata(bead beads.Bead, bdArgs []string) (beads.Bead, error) {
	if len(bdArgs) == 0 || bdArgs[0] != "update" {
		return bead, nil
	}
	metadata := make(beads.StringMap, len(bead.Metadata))
	for key, value := range bead.Metadata {
		metadata[key] = value
	}
	bead.Metadata = metadata
	edits, err := parseWorkRecordMetadataEdits(bdArgs)
	if err != nil {
		return bead, err
	}
	if err := applyWorkRecordMetadataEdits(bead.Metadata, edits); err != nil {
		return bead, err
	}
	return bead, nil
}

// parseWorkRecordMetadataEdits extracts the metadata mutations from a `bd update`
// arg list, matching bd's flag semantics: --metadata is a scalar whose last
// occurrence wins, and every known update flag's separate value is consumed so a
// value that itself looks like a metadata flag never mutates the prospective
// record. `--` terminates flag parsing.
func parseWorkRecordMetadataEdits(bdArgs []string) (workRecordMetadataEdits, error) {
	valueFlags := bdSubcmdValueFlags("update")
	var edits workRecordMetadataEdits
	for i := 1; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		switch {
		case arg == "--":
			i = len(bdArgs)
		case arg == "--metadata":
			if i+1 >= len(bdArgs) {
				return edits, fmt.Errorf("cannot project --metadata: missing JSON value")
			}
			i++
			edits.metadataJSON = bdArgs[i]
			edits.hasMetadataJSON = true
		case strings.HasPrefix(arg, "--metadata="):
			edits.metadataJSON = strings.TrimPrefix(arg, "--metadata=")
			edits.hasMetadataJSON = true
		case arg == "--set-metadata":
			if i+1 >= len(bdArgs) {
				return edits, fmt.Errorf("cannot project --set-metadata: missing key=value")
			}
			i++
			edits.setMetadata = append(edits.setMetadata, bdArgs[i])
		case strings.HasPrefix(arg, "--set-metadata="):
			edits.setMetadata = append(edits.setMetadata, strings.TrimPrefix(arg, "--set-metadata="))
		case arg == "--unset-metadata":
			if i+1 >= len(bdArgs) {
				return edits, fmt.Errorf("cannot project --unset-metadata: missing key")
			}
			i++
			edits.unsetMetadata = append(edits.unsetMetadata, bdArgs[i])
		case strings.HasPrefix(arg, "--unset-metadata="):
			edits.unsetMetadata = append(edits.unsetMetadata, strings.TrimPrefix(arg, "--unset-metadata="))
		case !strings.Contains(arg, "=") && valueFlags[arg] && i+1 < len(bdArgs):
			i++
		}
	}
	return edits, nil
}

// applyWorkRecordMetadataEdits overlays parsed edits onto metadata, matching bd:
// --metadata cannot be combined with the edit flags, and bd applies every
// --set-metadata edit before every --unset-metadata edit regardless of their
// order in argv. A more permissive projection could validate prospective
// metadata that bd never persists and allow an invalid close.
func applyWorkRecordMetadataEdits(metadata beads.StringMap, edits workRecordMetadataEdits) error {
	if edits.hasMetadataJSON && (len(edits.setMetadata) > 0 || len(edits.unsetMetadata) > 0) {
		return fmt.Errorf("cannot project metadata: --metadata cannot be combined with --set-metadata or --unset-metadata")
	}
	if edits.hasMetadataJSON {
		if err := mergeWorkRecordMetadataJSON(metadata, edits.metadataJSON); err != nil {
			return fmt.Errorf("cannot project --metadata: %w", err)
		}
		return nil
	}
	for _, edit := range edits.setMetadata {
		key, value, ok := strings.Cut(edit, "=")
		if !ok || key == "" {
			return fmt.Errorf("cannot project --set-metadata %q: expected key=value", edit)
		}
		metadata[key] = value
	}
	for _, key := range edits.unsetMetadata {
		if key == "" {
			return fmt.Errorf("cannot project --unset-metadata: key is empty")
		}
		delete(metadata, key)
	}
	return nil
}

// mergeWorkRecordMetadataJSON applies bd update's --metadata object as an
// additive metadata merge. Decode through beads.StringMap so the prospective
// bead sees the same boolean/number coercion as a bead read back from bd.
// @file inputs deliberately fail closed: resolving a caller-relative file in
// this preflight would introduce a second filesystem interpretation of bd's
// input and could validate bytes different from the mutation bd performs.
func mergeWorkRecordMetadataJSON(metadata beads.StringMap, value string) error {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "@") {
		return fmt.Errorf("@file input is not supported by the close gate")
	}
	var update beads.StringMap
	if err := json.Unmarshal([]byte(value), &update); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	for key, item := range update {
		metadata[key] = item
	}
	return nil
}
