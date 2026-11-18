---
worth: yes
where: scripts/ralphex-dk/ralphex_dk.py:447
added: 2026-08-13
---
# docker wrapper bind-mounts host directories chosen by repository content

`add_symlink_targets(cwd / ".ralphex")` (`scripts/ralphex-dk.sh:445-447`) walks the repository's own
`.ralphex/` two levels deep, resolves every symlink it finds, and read-only bind-mounts the resolved
target's **parent** at its absolute host path whenever that parent sits under `$HOME`
(`symlink_target_dirs`, `:192-217`; `add_symlink_targets`, `:384-388`).

Git stores symlinks, so `.ralphex/` content selects host mounts. A committed
`.ralphex/x -> ../../../../.aws/credentials` resolves to a parent of `~/.aws`, which is then mounted into a
container running an agent with `--dangerously-skip-permissions` and full network access. That contradicts
`README.md:406`, which states the container cannot access "SSH keys, AWS credentials, or other secrets in
`~/.ssh`, `~/.aws`". A symlink resolving to `$HOME/.config` yields parent `$HOME` — the whole home
directory.

The two sibling calls are fine and should stay: `:414` on `~/.claude` and `:420` on `~/.codex` walk
user-controlled directories, which is the feature's purpose (following a symlinked `~/.claude/skills`).
Only `:447` scans repository content.

Several defensible shapes for the fix, which is why it was not done inline: drop the repo-side walk
entirely (the workspace mount already covers `.ralphex/`, so only out-of-repo targets are affected); keep
it but refuse targets outside the repository; or keep it and warn. Worth deciding before touching the code.

Caveats on the evidence: mounts are read-only, and a relative symlink has to guess the repo's depth below
`$HOME` — though non-resolving variants are silently dropped by the `is_dir()` filter, so several can be
committed at once. The mount decision was verified by calling the helper directly; no container was run.

Surfaced investigating issue #431, which reported a different and already-declined trust question about
`.ralphex/config`. This one is independent of that report and is a mismatch with a documented claim.

## Current-source review and acceptance — 2026-11-18

The implementation now lives in
[`scripts/ralphex-dk/ralphex_dk.py`](../../scripts/ralphex-dk/ralphex_dk.py),
not the launcher named in the historical evidence above. At default-branch commit
`4b77ccdc97eb2b42deec3b9e759b6ca58b355611`, `symlink_target_dirs`, the
home-contained parent filter in `add_symlink_targets`, and the repository-local
call in `build_volumes` still form the same path. Source inspection only: this
review did not run the helper, start Docker or read real credentials.

Keep native `worth: yes`; prioritize this host-mount boundary before convenience
work. The unresolved choice is the repository-target policy, not whether a
read-only mount is a confidentiality boundary. A warning-only choice needs an
explicit owner risk decision; it does not satisfy fail-closed acceptance below.

Acceptance for a fail-closed implementation, without expanding the original scope:

- [ ] Repository-owned symlinks cannot implicitly add host directories outside the
  resolved workspace; exercise both file and directory targets and chained links.
- [ ] Isolated fake-home fixtures prove that an escaped target cannot select a
  secret-parent directory or the whole home, including the resolved parent mount
  rather than just the target itself. Do not use actual host secrets or containers.
- [ ] Broken links and symlink loops are handled deterministically without a broad
  fallback mount. Repository-contained targets and ordinary workspace use still work.
- [ ] Explicit user-controlled global Claude, Codex and ralphex configuration links
  retain their intended support; any explicit extra-volume opt-in remains distinct
  from mounts selected automatically by repository contents.
- [ ] Document the selected policy and ensure the resulting mount arguments match
  the documented isolation claim. Do not silently broaden credential access.
