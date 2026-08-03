# Release gate: align BEADS_ACTOR with the stable agent alias

- Deploy bead: `ga-d6zqfj`
- Build bead: `ga-jav9u9`
- Source review: `ga-mfccbk`
- Reviewed commit: `941812ce44b193ebfc3ab3903861bcf79467e3e3`
- Reviewed base: `1f948e67b0ac088492af67c0748f521aad5768b0`
- Main evaluated: `origin/main@3b4e9ea98a636f3cf2a0db5945f03de69e9fb651`
- Source branch: `builder/ga-jav9u9` (provenance and bounded-rebase target only)
- Planned deploy branch: `deploy/ga-d6zqfj-gate`
- Evaluated: `2026-08-03`
- Overall verdict: **FAIL**

`docs/PROJECT_MANIFEST.md` is not present at the evaluated commit, so this
checklist applies the deployer role's release-gate criteria together with
`engdocs/contributors/release-gate-criteria-conventions.md`.

Evaluation stopped after criterion 6 as required. No test suite, push of the
deploy branch, or pull request was attempted.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Branch diverges cleanly from main | **FAIL** | Evaluated first after `git fetch origin main`. `origin/main@3b4e9ea98a636f3cf2a0db5945f03de69e9fb651` was not an ancestor of reviewed commit `941812ce44b193ebfc3ab3903861bcf79467e3e3` (merge base `1f948e67b0ac088492af67c0748f521aad5768b0`). `git merge-tree --write-tree origin/main 941812ce44b193ebfc3ab3903861bcf79467e3e3` was conflict-free and produced tree `e2f8444bf610d36846c6ccdd1d5f46ed917a64e0`, so the permitted bounded helper was attempted on the internally authored source branch. It rebased locally to `08f5fff1d0499cba7a8963402915932b42ea19f2`, but returned `rc=14`: `push-ownership-guard` resolved the branch to closed build bead `ga-jav9u9` and blocked the required force-with-lease push. Remote `origin/builder/ga-jav9u9` therefore remains at `941812ce44b193ebfc3ab3903861bcf79467e3e3`. Per the guardrail, a locally rebased but unpushed source is not a criterion-6 PASS. |
| 1 | Review PASS present | **SKIPPED** | Fail-fast after criterion 6. The review bead was not promoted into gate evidence. |
| 2 | Acceptance criteria met | **SKIPPED** | Fail-fast after criterion 6; acceptance criteria were not re-evaluated. |
| 3 | Tests pass | **SKIPPED** | Fail-fast after criterion 6. The documented CI-equivalent command for this `cmd/gc/**` change is `make test-cmd-gc-process-parallel`, but it was intentionally not run on an unpushable rebased source. PASS/FAIL/SKIP counts are therefore not claimed. |
| 4 | No high-severity review findings open | **SKIPPED** | Fail-fast after criterion 6. |
| 5 | Final branch is clean | **SKIPPED** | Fail-fast after criterion 6. The helper left the temporary source worktree clean before this checklist was written, but cleanliness is not promoted to a gate PASS after criterion 6 failed. |
| 7 | Single feature theme | **SKIPPED** | Fail-fast after criterion 6. |

## Required remediation

The builder must re-establish a pushable source branch on current `origin/main`
and return it for review/deploy. The deployer must then rerun the complete gate,
including `make test-cmd-gc-process-parallel`, before preparing an isolated
`deploy/ga-d6zqfj-gate` branch or opening a pull request.
