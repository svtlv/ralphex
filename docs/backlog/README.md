# Backlog navigation

Reviewed against the `windows` default branch on 2026-11-18. Individual records
remain canonical for requirements and native `worth: yes/maybe/later` decisions.
This ordering is a grooming recommendation, not a new implementation plan or a
change to upstream policy. No item is complete merely because a related PR exists.

## First: host-mount boundary (`worth: yes`)

1. [Repository-selected host mounts](docker-wrapper-mounts-repo-chosen-host-dirs.md).
   Security-boundary work takes precedence within the actionable queue: decide a
   fail-closed repository-target policy and verify isolated mount-argument fixtures.
   Retain explicitly user-controlled global symlinks. No container or secret access
   is needed to test argument selection. See the record's current-source acceptance.

## Next: actionable fixes (`worth: yes`)

2. [Filter custom-review session markers](custom-review-script-inherits-session-env.md):
   preserve intentional authentication passthrough; cover child-env isolation.
3. [External authentication without local Claude credentials](wrapper-blocks-users-without-local-claude-creds.md):
   define the provider-neutral credential contract before documenting a working route.
4. [Gateway environment documentation](gateway-env-route-undocumented.md): depends
   on resolving the preceding gate for users without a local login.
5. [Actual provider signal names](custom-providers-names-nonexistent-signals.md):
   verify both documentation locations against `pkg/status/status.go`.
6. [Compile the tagged e2e package in CI](ci-never-compiles-e2e-package.md): keep
   browser execution manual; a compile gate is not a browser run.
7. [Module-selected Playwright installer](playwright-driver-install-uses-latest.md):
   reconcile the Makefile, manual workflow and local instructions together.
8. [Correct the init-docker CI claim](claude-md-claims-init-docker-suite-not-in-ci.md):
   the suite is already invoked by CI; correct documentation rather than add a gate.
9. [Reject unknown pi thinking levels](pi-ignores-unknown-thinking-level.md):
   blocked on refreshing upstream behavior/choosing a fork-side mitigation; do not
   confuse it with effective-level observability. No upstream write is implied.
10. [Archive a plan from an ignored plans folder](plan-archive-fails-in-ignored-plans-dir.md):
   move the file without add or commit when git ignores it; today the run ends with an error.
11. [Codex model banner on every call](codex-model-banner-shown-only-on-first-call.md):
   show the resolved model and effort, and agrouter's selection line, for each codex call.

## Design decisions (`worth: maybe`, not rejected)

- [OAuth-token/keychain precedence](wrapper-reads-keychain-despite-oauth-token.md)
  and [mocked Darwin extraction coverage](keychain-extraction-path-untested.md):
  coordinate with external authentication, retaining each distinct acceptance set.
- [Codex transient retry tier](no-codex-retry-tier.md) and
  [original diagnostic preservation](pattern-error-discards-cli-diagnostic.md):
  related error-policy decisions, not duplicate outcomes.
- [Plan prompt bare-signal rule](make-plan-prompt-lacks-bare-signal-rule.md):
  distinguish PLAN_READY from payload-bearing QUESTION/PLAN_DRAFT.
- [Mid-operation worktree guard](worktree-guard-refuses-mid-operation-checkout.md):
  deliberate conservative behavior; do not weaken it as routine cleanup.

## Deferred (`worth: later`, not obsolete)

- [Diagnostic aging](diagnostic-window-evicted-only-by-diagnostics.md)
- [Executor helper conventions](executor-helpers-called-only-from-methods.md)
- [Effective pi thinking level](pi-effective-thinking-level-unobservable.md)
- [Unobserved diagnostic classes](system-error-diagnostic-branches-unverified.md)
- [Watcher test shutdown joins](watcher-tests-never-join-start-goroutine.md)
- [Durable completed-worktree progress](worktree-archives-unticked-plan.md)
- [Worktree configuration precedence](worktree-ignores-its-own-ralphex-config.md)
- [Claude review rounds repeat research](claude-review-rounds-repeat-research.md)

## Existing plan boundary

[Long command-line transport plan](../plans/long-lines-plan.md) retains unchecked
wrapper and verification work. Existing Codex stdin transport does not complete
Gemini/OpenCode, wrapper verification or full-suite acceptance. Preserve its task
IDs and checked state; do not archive it based on the Windows support PR alone.
