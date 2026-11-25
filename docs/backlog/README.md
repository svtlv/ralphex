# Backlog navigation

The linked records contain the requirements and the `worth` decisions. This order is a recommendation. It does not change upstream policy or set a schedule.

## First: host-mount boundary (`worth: yes`)

1. [Repository-selected host mounts](docker-wrapper-mounts-repo-chosen-host-dirs.md).
   Select a fail-closed policy for repository targets. Keep user-controlled global symlinks. Verify the mount arguments with isolated test data.

## Next: actionable fixes (`worth: yes`)

2. [External authentication without local Claude credentials](wrapper-blocks-users-without-local-claude-creds.md).
   Define the provider-neutral credential requirements before you document an authentication route.
3. [Gateway environment documentation](gateway-env-route-undocumented.md).
   This depends on the external authentication decision.
4. [Reject unknown pi thinking levels](pi-ignores-unknown-thinking-level.md).
   Check upstream behavior before you select a fix for this fork. This is separate from reporting the effective thinking level.

## Design decisions (`worth: maybe`, not rejected)

- [OAuth token and keychain order](wrapper-reads-keychain-despite-oauth-token.md).
  Coordinate this decision with external authentication.
- [Codex retry policy](no-codex-retry-tier.md) and [original diagnostic text](pattern-error-discards-cli-diagnostic.md).
  The decisions are related. The required results are different.
- [Mid-operation worktree guard](worktree-guard-refuses-mid-operation-checkout.md).
  Do not remove the guard as routine cleanup.

## Deferred (`worth: later`, not obsolete)

- [Diagnostic aging](diagnostic-window-evicted-only-by-diagnostics.md)
- [Executor helper conventions](executor-helpers-called-only-from-methods.md)
- [Effective pi thinking level](pi-effective-thinking-level-unobservable.md)
- [Unobserved diagnostic classes](system-error-diagnostic-branches-unverified.md)
- [Watcher test shutdown joins](watcher-tests-never-join-start-goroutine.md)
- [Durable completed-worktree progress](worktree-archives-unticked-plan.md)

## Existing plans

The [long command-line transport plan](../plans/long-lines-plan.md) still has unchecked work. Existing Codex stdin transport does not complete Gemini or OpenCode transport, wrapper verification, or the full test suite.

## Records no longer in this list

[Upstream PR #483](https://github.com/umputun/ralphex/pull/483) removed eight records. The [commit](https://github.com/svtlv/ralphex/commit/cd1ca6b18958bc2b4bdae71657b412611dcd6e3a) shows the changes and the deleted records. The CI job checks the tagged package with `go vet`. It does not run the browser tests.
