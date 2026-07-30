# Automatic `needs-mac` Label Plan

Root bead: `ga-77idy2`  
Architecture source: `ga-hd99jq` D2, especially AF3 and AF4  
Incident source: `ga-xbilek` / PR #4844 / `d2142785f`

## Outcome

Proceed with a narrow, add-only path policy only if `ga-77idy2.4` identifies a
safe end-to-end event chain. The path and lifecycle policy is settled below,
but the implementation is not builder-ready yet.

The architecture handoff assumed that a workflow could add `needs-mac` with
the repository `GITHUB_TOKEN` and thereby wake
`dispatch-labeled-pr-suite.yml`. [GitHub's current `GITHUB_TOKEN`
documentation](https://docs.github.com/en/actions/concepts/security/github_token#when-github_token-triggers-workflow-runs)
states that token-caused `pull_request` `labeled` activity does not create a
workflow run. The original `pull_request` event also contains the pre-label
payload. A one-push PR could therefore display `needs-mac` while Mac Regression
never runs for that revision.

The automation is a go only if the architecture child can preserve the visible
label and existing trust boundary without exposing write credentials to PR
code or silently duplicating authorization logic. Otherwise the outcome is to
keep `needs-mac` manual and document the curated candidate list.

## Why the path list is narrow

The original candidate `cmd/gc/**` is too broad. The current tree contains 939
files under `cmd/gc`, and 1,297 distinct commits touched that directory in the
90-day retrospective window. The proposed initial policy covers 51 current
files and would have matched at most 81 distinct commits in the same window.
Those commit counts are an upper bound on qualifying PR revisions, not a
forecast of runner invocations, but they show why package-wide `cmd/gc`
matching is not acceptable for a scarce Mac lane.

Keep the whole of `internal/pathutil/**` and `internal/fsys/**`. Both are small,
cohesive packages whose purpose is path and filesystem behavior, so per-file
selection would be fragile without materially reducing the policy.

Use this initial allowlist:

```text
internal/pathutil/**
internal/fsys/**
internal/testutil/path.go
cmd/gc/path_*.go
cmd/gc/*_path*.go
cmd/gc/city_discovery*.go
cmd/gc/*worktree*.go
cmd/gc/cmd_supervisor_city*.go
.github/actions/setup-gascity-macos/**
**/*_darwin.go
**/*_darwin_test.go
```

Important negative controls:

- `cmd/gc/**` as a whole
- `cmd/gc/api_state*.go`, which is high-churn and the cited symlink defect is
  cross-platform rather than Mac-only
- generic `cmd/gc/*reaper*.go`, which sweeps unrelated cleanup mechanisms
- docs-only changes
- ordinary workflow changes, including
  `.github/workflows/dispatch-labeled-pr-suite.yml`

The original incident remains a mandatory positive fixture:
`cmd/gc/bead_worktree_reaper.go` matches through `cmd/gc/*worktree*.go`.

## Label lifecycle policy

- Evaluate the current net PR diff on `opened`, `reopened`, `synchronize`, and
  `ready_for_review`.
- Automatic application is limited to same-repository PRs. Fork PRs retain the
  existing manual-label path through the trusted dispatcher.
- A matching draft may receive the label, but existing draft gates must prevent
  Mac runner use until the PR becomes ready.
- Add `needs-mac` only when it is absent. An existing label is an idempotent
  no-op.
- Never auto-remove `needs-mac`. The workflow cannot reliably distinguish a
  human forcing Mac coverage from its own earlier label.
- A human can remove the label. A later matching `synchronize` event may add it
  again because each new revision is reevaluated.
- Every run reports one clear outcome: matched, already labeled, unmatched,
  draft-gated, or fork/manual fallback, including the matching policy family.

## Permission and trust boundary

For label mutation alone, `pull-requests: write` is sufficient;
`contents: read` is the only additional permission that may be needed for the
chosen path-evaluation mechanism. Those permissions are not, by themselves,
an end-to-end solution: a label written with `GITHUB_TOKEN` does not wake the
existing `pull_request_target: labeled` dispatcher.

`ga-77idy2.4` must decide the complete event chain and specify:

- token provenance and minimum permissions;
- whether workflow code comes from the trusted base or PR head;
- same-repo, fork, and draft behavior;
- how the first qualifying revision dispatches exactly once;
- concurrency and duplicate-run behavior; and
- whether a GitHub App, PAT, secret, or operator action is required.

The architecture child may choose a manual-only outcome. Downstream agents must
not invent a direct dispatch, new secret, or second trust check to work around
that decision.

## Child beads

| Order | Bead | Route | Purpose |
| --- | --- | --- | --- |
| 1 | `ga-77idy2.4` | `gascity/architect` | Resolve the `GITHUB_TOKEN` label-event suppression and issue a buildable or manual-only decision |
| 2 | `ga-77idy2.1` | `gascity/validator` | Pin the approved end-to-end chain, narrow path matrix, lifecycle, and security invariants |
| 3 | `ga-77idy2.2` | `gascity/builder` | Implement the approved automatic-label path, or close superseded on a manual-only decision |
| 4 | `ga-77idy2.3` | `gascity/builder` | Document automatic behavior after implementation, or document the manual policy if automation is deferred |

Dependency graph:

```text
ga-77idy2.4
  -> ga-77idy2.1
       -> ga-77idy2.2
            -> ga-77idy2.3
```

Only `ga-77idy2.4` is ready to dispatch now. The validator had already claimed
`ga-77idy2.1` during the interrupted first PM pass and has been notified to
pause on the new architecture dependency.

Duplicate children `ga-77idy2.5` and `ga-77idy2.6` were created during recovery
before the earlier children were discovered; both are closed as superseded.

## Acceptance summary

The work is complete when:

- a qualifying first same-repo PR revision causes the intended Mac Regression
  run, not merely a visible label;
- the tested allowlist catches the `ga-xbilek` path while unrelated
  `cmd/gc` work stays unmatched;
- new sensitive paths introduced by a later push are caught;
- the workflow never auto-removes a human-forced label;
- fork and draft behavior preserve the existing security and cost controls;
- no path hit directly flips `run_smoke` or `run_full`;
- no change touches ruleset 14017226;
- `ga-n7ef4e` retains ownership of `mac-regression.yml` gate mechanics; and
- the contributor design documentation matches the final architecture outcome.

## Out of scope

- Changing `mac-regression.yml` trigger or tier-gate logic
- Adding Mac Regression to required checks
- Modifying ruleset 14017226
- Direct path-based Mac gate bypass
- Automatically removing `needs-mac`
- Auto-labeling fork PRs without an explicit architecture revision

## Planning-system note

The repository moved internal PM artifacts to `engdocs/plans` in `512b066e8`.
The active PM prompt still names `docs/plans`; open bead `ga-f74ph9.1` owns that
pack-level correction. This artifact follows the current repository decision.
