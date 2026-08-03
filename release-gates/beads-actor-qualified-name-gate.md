# Release gate: align BEADS_ACTOR with the stable agent alias

- Deploy bead: `ga-d6zqfj`
- Build bead: `ga-jav9u9`
- Source review: `ga-mfccbk`
- Reviewed commit: `941812ce44b193ebfc3ab3903861bcf79467e3e3`
- Gated source tip: `6384e9a82b0950a219041fa0efe121dec40aea8b`
- Main evaluated: `origin/main@85e3e5022b925c9781fb64e0b1a043133770cf72`
- Source branch: `builder/ga-jav9u9`
- Planned deploy branch: `deploy/ga-d6zqfj-gate`
- Evaluated: `2026-08-03`
- Overall verdict: **FAIL**

`docs/PROJECT_MANIFEST.md` is not present at the evaluated commit. This
checklist therefore applies the deployer role's seven release criteria and
`engdocs/contributors/release-gate-criteria-conventions.md`.

The builder rebased the reviewed RED and GREEN commits onto current main after
the prior gate attempt. Their stable patch IDs are unchanged (`1069edfc...` and
`0f232508...` respectively), so the gated source contains the same reviewed
feature patch plus the prior gate record.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first after `git fetch origin main builder/ga-jav9u9`. `origin/main@85e3e5022b925c9781fb64e0b1a043133770cf72` is an ancestor of the clean source tip `6384e9a82b0950a219041fa0efe121dec40aea8b`, which exactly matched `origin/builder/ga-jav9u9`; merge base was the current main tip and `git diff --check origin/main...HEAD` was clean. No bounded self-rebase was required. |
| 1 | Review PASS present | **PASS** | Review bead `ga-mfccbk` is closed with `close_reason=pass`, `verdict: pass`, and reviewed commit `941812ce44b193ebfc3ab3903861bcf79467e3e3`. The rebased RED/GREEN commits have stable patch IDs identical to the reviewed commits. |
| 2 | Acceptance criteria met | **PASS** | `cmd/gc/template_resolve.go` sets `BEADS_ACTOR` to `qualifiedName`, matching `GC_ALIAS`, rather than the per-session `sessName`. `TestResolveTemplateSetsBeadsActorToQualifiedNameNotSessionName` passed independently: 1 PASS, 0 FAIL, 0 SKIP. |
| 3 | Tests pass | **FAIL** | Required path-matched local CI equivalent `make test-cmd-gc-process-parallel`: 4/7 jobs PASS (`cmd-gc-process` shards 1, 3, 6 plus `productmetrics-testhook`), 3/7 FAIL (shards 2, 4, 5), 0 jobs SKIP; exit 123. Failing tests were `TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession`, `TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork`, and `TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork`. Two failures explicitly report `database "beads" not found on Dolt server at 127.0.0.1:3308`; the third reports its generated `bd ready` command exiting 1. Logs: `/var/tmp/gc-local-tests.r4VqAA/cmd-gc-process-{2,4,5}-of-6.log`. Per test-evidence policy, this red required run cannot be promoted to PASS even though the feature-focused test is green. Remaining path-matched integration and worker-core lanes were not run after the required process suite failed. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-mfccbk` records no style or security findings and no blocker/major/minor security issue; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | The source worktree was clean at `6384e9a82b0950a219041fa0efe121dec40aea8b` before this checklist was written; the gate record is committed below as the only deployer change. |
| 7 | Single feature theme | **PASS** | The substantive diff is confined to `cmd/gc` agent-environment identity construction, its focused regression test, and one directly related test comment. The prior gate record is release evidence for the same feature, not an independent theme. |

## Required remediation

The builder must make the documented `cmd/gc` process suite pass on the exact
source tip, including isolating the three pool tests from the ambient Dolt
endpoint or otherwise proving and repairing the failing test environment. The
deployer must rerun the full gate before creating or pushing an isolated deploy
branch. No deploy branch was pushed and no pull request was opened.
