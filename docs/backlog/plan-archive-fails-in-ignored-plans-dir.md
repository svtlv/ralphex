---
worth: yes
where: pkg/git/service.go:525
added: 2026-11-19
---
# archiving a plan from a git-ignored plans folder fails the end of the run

When `plans_dir` (for example `docs/plans/`) is in `.gitignore`, `git mv` refuses the plan, so
`MovePlanToCompleted` falls back to `os.Rename`, logs a failed `add` as a warning, and then returns
`commit plan move` as an error: there is nothing it may stage. The run did all its work but ends with an
error, and the plan sits in `completed/` uncommitted. Seen on a Windows run for ATH-2386 (autopilot
field report, item 10).

Fix: check `git check-ignore` on the source and destination before the move. When both are ignored,
move the file only and skip the add and the commit (log one line that the plan is not in git). Same
root cause as autopilot's `--new-worktree` not committing an ignored plan, which autopilot now copies
without a commit and reports.
