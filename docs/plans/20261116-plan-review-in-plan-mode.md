# Native plan review for plan mode and a standalone plan-review mode

## Overview
After a plan file is written, ralphex offers a review of that file. The user picks one reviewer: External reviewer (codex or a custom script), Same agent (the plan executor, read-only), or Skip review. The reviewer reads the plan and the code. It prints one finding per line as `CRITICAL|MAJOR|MINOR: <section> - <text>`, or `NO ISSUES FOUND`. ralphex parses the findings from stdout only and shows them numbered. It then asks which findings to apply ("1,3", "all", "none"). The chosen findings go to a planner session as revision feedback. That session edits the plan file and emits PLAN_READY again. The offer repeats until the user picks Skip review, the review finds no issues, or the round cap is reached.

There are two entry points and one implementation:
- `--plan "description"` (ModePlan): the review runs after PLAN_READY. A run that continues into implementation gets the review before the tasks.
- `--plan-review [plan-file]` (new ModePlanReview): reviews an existing plan file. It asks no planner questions, makes no drafts, and creates no branch or worktree. With no file argument, the existing fzf plan selector picks the plan. After the loop it asks "Continue with plan implementation?", the same as plan mode.

The reviewer choice is preselected with config `plan_reviewer` and CLI flag `--plan-reviewer` (values ask, external, same, none; default ask). Executing an existing plan directly (full, tasks-only, review modes) runs no plan review.

The read-only rule is enforced twice. The prompt tells the reviewer not to edit. A repo guard compares git HEAD, `git status --porcelain` and file content hashes before and after the review. This matters because the same-agent claude reviewer runs with `--dangerously-skip-permissions` in the main checkout, so the prompt alone cannot stop an edit or a commit. A failed or timed-out review never blocks the plan: ralphex logs a warning and continues.

## Context
- Files involved: `pkg/processor/phase/plan_creation.go`, `pkg/processor/phase/phase.go`, `pkg/processor/phase/external_review.go` (Tool() logic), `pkg/processor/runner.go`, `pkg/processor/executor_factory.go`, `pkg/processor/prompt_builder.go`, `pkg/processor/prompts.go`, `pkg/config/config.go`, `pkg/config/values.go`, `pkg/config/prompts.go`, `pkg/config/defaults/config`, `pkg/config/defaults/prompts/`, `pkg/progress/progress.go`, `pkg/plan/`, `cmd/ralphex.windows/main.go`
- Related patterns: consumer-side interfaces in `pkg/processor/phase`; late-bound setters through `phase.Deps`; `AskQuestion` (fzf or numbered list) for all prompts; `LogQuestion` and `LogAnswer` for progress-file history; Go-side prompt blocks like `reviewContextInstruction`
- The planner writes the plan file itself, so ralphex does not know its path in ModePlan. The runner finds it with `plan.Selector.FindRecent(startTime)`, the same call `runPlanMode` uses. In ModePlanReview the path comes from the `--plan-review` argument or the selector.
- The codex executor returns only stdout in `Result.Output` (stderr goes to the log filter), so codex reviewer output is already stdout-only. The parser keeps only lines that match the finding format and drops exact duplicates. This also protects custom scripts that merge stderr into stdout.
- Routing: the external reviewer uses `execs.External` (built from `codex_command`, `codex_model`, `codex_reasoning_effort`, `codex_sandbox`, error and limit patterns) or `execs.Custom`. The same-agent reviewer uses `execs.Task`, built from `claude_command`, `claude_args` and `plan_model`, or from the codex executor under `--codex`. No code calls a `claude` or `codex` binary by name, so agrouter and wrapper scripts work.
- Design decisions:
  - One reusable `PlanReviewPhase` in `pkg/processor/phase` holds the whole loop. `PlanCreationPhase` stays free of review code. `Runner` calls the review phase after plan creation in ModePlan, and directly in ModePlanReview.
  - Under `--codex`, `external_review_tool` is forced to `none`, so External reviewer is not offered. `plan_reviewer = external` under `--codex` logs a warning and skips the review.
  - When the external tool is unavailable (`none`, no custom script, no executor), External reviewer is hidden. `plan_reviewer = external` then logs a warning and skips the review. Skip review is always present.
  - Repo guard: before the review, record the plan file bytes, git HEAD, the `git status --porcelain -uall -z` entries (individual files, not collapsed untracked directories; `-z` keeps quoted paths exact, as `pkg/git/external.go` already does with `-uall`), and a content hash for every listed file (files over 1 MiB use size and mtime). Paths are relative to the repo root (`RepoStateReader.Root()`), not the CWD, which also covers worktree mode. Paths under `.ralphex/` are skipped, because the progress file grows during the review. After the review, compare. Content hashes catch edits to files that were already dirty, where the status line does not change.
  - On any change: log a clear warning that names the changed paths (and the old and new HEAD if it moved), discard that round's findings, and end the review without error. ralphex does not revert code files or commits. It restores only the plan file, which it owns, from the stored bytes if the reviewer changed it; a restored plan path is not listed again in the changed-paths warning.
  - Known limit of the guard (documented, no toggle): the guard cannot tell a reviewer edit from a user edit or commit in another terminal during a review, so such an edit also discards that round's findings with a warning.
  - If the repo state cannot be read (not a git repo, VCS command fails), log a warning and run the review with the plan bytes check only.
  - External reviewer availability depends on `CodexEnabled` and `ExternalReviewToolSet` in `processor.Config`: `ExternalReviewPhase.Tool()` returns `none` when `CodexEnabled` is false. `runPlanMode` does not set them today (only `executePlan` does, `main.go:1060`), so both plan-mode `processor.Config` literals must pass `CodexEnabled: req.Config.CodexEnabled` and `ExternalReviewToolSet: o.externalReviewToolSet`; otherwise External reviewer is always hidden.
  - The custom reviewer runs through the same adapter that `ExternalReviewPhase.runReviewTool` uses (`CustomExecutor.Run` writes the prompt to a temp file and passes the path), not through a direct `policy.Run` call.
  - Executors already stream every output line to `log.PrintAligned` through `OutputHandler` (`executor_factory.go`), so the review phase does not log the reviewer output again; it logs only the parsed numbered findings, the questions, the answers and warnings.
  - Pattern errors (rate limit, quota) from a reviewer or a revision session go through `HandlePatternMatchError` / `wrapExecutorError` first, like other phases, so a limit waits or exits cleanly. Only the remaining errors become a warning.
  - Each review round prints a generic section header (`status.NewGenericSection`, for example `plan review round 1`) under `PhasePlan`, so the dashboard and progress replay show the rounds.
  - The review phase has its own consumer-side interfaces (`PlanReviewLogger`, `PlanReviewPrompts`, `PlanFinder`, `RepoStateReader`); the plan-creation interfaces are not widened.
  - The guard wraps only the reviewer call, not the planner revision session, which edits files by design.
  - The revision prompt is a Go-side block, not a new prompt file. `make_plan.txt` Step 0 exits at once when the plan exists, so it cannot drive a revision.
  - Review rounds run inside the phase and do not use the plan iteration counter. A cap of 5 rounds prevents an endless loop with a preselected reviewer.
  - Loop end conditions: Skip review chosen, review reports NO ISSUES FOUND, the user applies no findings, a warning path (failure, guard change, unparseable output), or the cap. In ask mode the reviewer question repeats after each applied revision. With a preselected reviewer there is no repeat question; the loop runs again only after an applied revision.
  - ModePlanReview takes the task description for the revision session from the plan's Overview section (no `--plan` text exists). Its progress file follows the existing naming pattern: `progress-<plan-stem>-plan-review.txt` (mode suffix `-plan-review`).
  - ModePlanReview reuses the planner executor settings of plan mode (`plan_model` falling back to `task_model`, codex executor under `--codex`).
- Prompt texts (stable, documented):
  - Reviewer question: `Review the plan before implementation?` with options `External reviewer`, `Same agent`, `Skip review`. External reviewer is listed first when it is available (`AskQuestion` has no recommended marker; it only appends "Other"). Unavailable options are omitted.
  - Findings question: `Which findings should be applied?` with options `All findings`, `None`. The user can use the Other option to type numbers such as `1,3` or `2-4`.
  - Continue question (both entry points): `Continue with plan implementation?` (existing `AskYesNo`).
- Dependencies: none new

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- Table-driven tests with testify; lowercase comments; one `_test.go` per source file
- **CRITICAL: every task MUST include new/updated tests**
- **CRITICAL: all tests must pass before starting next task**
- Tests must use `t.TempDir()` and never touch `~/.config/ralphex/`
- Mocks for the new consumer-side interfaces (`PlanFinder`, `RepoStateReader`, `PlanReviewLogger`, `PlanReviewPrompts`) are generated with moq into `pkg/processor/phase/mocks/` through `//go:generate moq -out mocks/<name>.go -pkg mocks -skip-ensure -fmt goimports . <Interface>` lines (same form as `pkg/processor/runner.go`); existing package-local fakes for the executor and input collector may be reused
- Do not edit CHANGELOG.md. Do not edit plans in `docs/plans/completed/`.

## Implementation Steps

### Task 1: Config option plan_reviewer and plan_review.txt prompt

**Files:**
- Modify: `pkg/config/config.go`, `pkg/config/values.go`, `pkg/config/prompts.go`, `pkg/config/defaults/config`
- Create: `pkg/config/defaults/prompts/plan_review.txt`

- [x] add `plan_reviewer` to `Values` and `Config` (`PlanReviewer string`); parse it in `values.go`; merge local over global; validate it is one of ask, external, same, none (case-insensitive, empty means ask) and return a clear error otherwise
- [x] add `plan_reviewer = ask` with a comment block to `pkg/config/defaults/config`; the comment lists the four values and says that `ask` offers External reviewer first
- [x] add `planReviewPromptFile`, `Prompts.PlanReview`, `Config.PlanReviewPrompt` and the loading step (local, then global, then embedded)
- [x] write `plan_review.txt`: leading meta-comment with variables `{{PLAN_FILE}}`, `{{PLAN_DESCRIPTION}}`, `{{PROGRESS_FILE}}`, `{{DEFAULT_BRANCH}}`; checklist of wrong assumptions about the code, missing steps, over-engineering, task order, wrong validation commands, CLAUDE.md conventions; state that the fixed plan format (`### Task N:` and `### Iteration N:` headers, checkboxes only in Task sections, Overview, Context) is not a finding; require read-only work (no file edits, no commits, no staging); require output as one line per finding `CRITICAL|MAJOR|MINOR: <section> - <text>` or exactly `NO ISSUES FOUND`; tell the reviewer to verify claims in the code
- [x] write tests: parsing, default, merge and invalid value for `plan_reviewer`; prompt fallback local, global, embedded; installer test lists the new file (see `defaults_test.go`)
- [x] run `go test ./pkg/config/...` - must pass before task 2

### Task 2: CLI flags, mode constant and progress naming

**Files:**
- Modify: `cmd/ralphex.windows/main.go`, `cmd/ralphex.windows/main_test.go`, `pkg/processor/runner.go`, `pkg/progress/progress.go`, `pkg/progress/progress_test.go`

- [x] add `ModePlanReview Mode = "plan-review"` next to the other modes in `runner.go` (dispatch comes in task 7; until then `determineMode` can return it while `Runner.Run` reports "unknown mode" — expected, no test runs that path before task 7)
- [x] add `PlanReview bool` with `long:"plan-review"` (existing plan file as the positional argument, optional) and `PlanReviewer string` with `long:"plan-reviewer"` (values ask, external, same, none) to `opts`
- [x] `determineMode` returns `ModePlanReview` for `--plan-review`; add `!o.PlanReview` to `isResetOnly`; `tryAutoPlanMode`: no change (that path is unreachable for `--plan-review`)
- [x] `validateFlags`: `--plan-review` conflicts with `--plan`, `--review`, `--external-only`, `--codex-only`, `--tasks-only` (clear error each); `--plan-reviewer` must be one of the four values; `--plan-review` with `--plan-reviewer=none` is an error
- [x] `applyCLIOverrides`: `--plan-reviewer` overrides `cfg.PlanReviewer`; after the merge, reject standalone `--plan-review` when the effective reviewer is `none` (covers `plan_reviewer = none` in config)
- [x] `pkg/progress`: add the `plan-review` case to `modeSuffix` (`-plan-review`); `progressFilename` then gives `progress-<stem>-plan-review.txt` because `PlanFile` is always set in this mode (no no-plan fallback change); update the mode comment in `Config`
- [x] write tests: mode selection, every flag conflict, invalid and valid `--plan-reviewer`, none with `--plan-review`, CLI over config precedence, `isResetOnly`, progress filename for the new mode
- [x] run `go test ./cmd/... ./pkg/progress/...` - must pass before task 3

### Task 3: Prompt builders and plan Overview helper

**Files:**
- Modify: `pkg/processor/prompt_builder.go`, `pkg/processor/prompts.go`, `pkg/processor/prompts_test.go`, `pkg/processor/prompt_builder_test.go`, `pkg/processor/phase/phase.go`, `pkg/plan/parse.go`, `pkg/plan/parse_test.go`

- [x] add `PlanReviewPrompt(planFile, description string) string` to the prompt builder; replace `{{PLAN_FILE}}` with the explicit plan path and `{{PLAN_DESCRIPTION}}` before the base variable pass (the locator has no plan file in plan mode); then the base variables
- [x] add `PlanRevisionPrompt(planFile, description, findings string) string`; a Go constant template that tells the planner: the plan file already exists at the path; read the progress file for earlier Q&A; apply only the listed findings by editing that file in place; keep the plan format (Task and Iteration keywords stay English; checkboxes only in Task sections); do not create a new file; do not emit PLAN_DRAFT or QUESTION; end with PLAN_READY; no markdown in response text
- [x] add `plan.ExtractOverview(path string) (string, error)` (or the nearest existing helper) that returns the text of the `## Overview` section, falling back to the first heading; used as the description in ModePlanReview
- [x] define a new consumer-side `PlanReviewPrompts` interface in `phase.go` with both builder methods; do not widen the existing plan-creation prompt interface
- [x] write tests for variable replacement (including a plan path that differs from the locator), the commit trailer, the revision text, and Overview extraction (present, missing, single-hash plans)
- [x] run `go test ./pkg/processor/... ./pkg/plan/...` - must pass before task 4

### Task 4: Findings parser, selection parser and repo guard

**Files:**
- Create: `pkg/processor/phase/plan_review.go`, `pkg/processor/phase/plan_review_test.go`, `pkg/processor/phase/plan_review_guard.go`, `pkg/processor/phase/plan_review_guard_test.go`
- Modify (only if no fitting method exists): `pkg/git/service.go`, `pkg/git/external.go`

- [ ] add `parsePlanFindings(output string) (findings []string, noIssues bool)`; match lines that start with CRITICAL, MAJOR or MINOR followed by a colon (allow a list marker and `**` bold around the label); keep the order; drop exact duplicates; set `noIssues` when the output has `NO ISSUES FOUND` and no findings
- [ ] add `parseFindingSelection(answer string, n int) ([]int, error)`; accept `all`, `all findings`, `none`, `apply none` (case-insensitive), and lists such as `1,3`, `1 3`, `2-4`; reject out-of-range numbers and unknown text with a clear error
- [ ] add `formatFindingsFeedback(findings []string, selected []int) string` that builds the numbered feedback text for the planner
- [ ] add a consumer-side `RepoStateReader` interface in the guard file (`Root() string`, `Head() (string, error)`, `StatusPorcelain() (string, error)`); `git.Service.Root()` already exists; reuse an existing `pkg/git` method for the rest if one fits, otherwise add small methods on `git.Service` that run `rev-parse HEAD` and `status --porcelain -uall -z` through the configured VCS command (so `vcs_command` wrappers keep working); add the moq `//go:generate` line for it
- [ ] add `snapshotRepo(reader, planFile) (repoSnapshot, error)`: HEAD, the NUL-separated status entries (individual files only, no directory case), a sha256 for each listed file under 1 MiB read from `reader.Root()` joined with the git path (size and mtime for larger files), and the plan file bytes; skip every path under `.ralphex/`
- [ ] add `diffSnapshots(before, after repoSnapshot) (changed []string, headMoved bool)` that returns sorted changed paths (added, removed, status changed or content changed) and whether HEAD moved; add `restorePlan(snapshot)` that writes the stored plan bytes back and lets the caller drop the plan path from the changed-paths list
- [ ] write table-driven tests with the moq reader and `t.TempDir()` files: no change; a new file; a new file inside an untracked directory; an edit to an already-dirty file (same status line, new hash); a deleted file; a staged file; a path with spaces or quotes (`-z`); a change under `.ralphex/` is ignored; HEAD moved by a commit; plan file changed and restored from bytes; git paths resolved against `Root()` from a non-root CWD; reader error returns an error the caller can downgrade to plan-bytes-only; findings parser cases (doubled stdout, prose mixed with findings, bold labels, no-issues output, unparseable output); every selection form and every error case
- [ ] run `go test ./pkg/processor/phase/... ./pkg/git/...` - must pass before task 5

### Task 5: Dependencies and wiring for the review phase

**Files:**
- Modify: `pkg/processor/phase/external_review.go`, `pkg/processor/phase/phase.go`, `pkg/processor/runner.go`, `pkg/processor/executor_factory.go`
- Modify tests: `pkg/processor/phase/external_review_test.go`, `pkg/processor/runner_test.go`, `pkg/processor/executor_factory_test.go`

- [ ] extract the external tool resolution from `ExternalReviewPhase.Tool()` into a shared function in package `phase`; keep `Tool()` as a thin caller so behavior does not change
- [ ] add consumer-side `PlanFinder` (`FindRecent(time.Time) string`) and `RepoStateReader` fields to `phase.Deps`; add `Runner.SetPlanFinder` and `Runner.SetRepoState` setters
- [ ] executor factory: no code change needed — it already builds `External` and `Custom` for every non-codex mode and leaves `External` nil under `--codex` (`executor_factory.go:24,41`); only add the test below
- [ ] define a new consumer-side `PlanReviewLogger` interface for the review phase (with `PrintAligned`, `LogQuestion`, `LogAnswer`, warnings, section printing); do not widen `PlanCreationLogger`; add moq `//go:generate` lines for `PlanReviewLogger`, `PlanReviewPrompts` and `PlanFinder` and run `go generate`
- [ ] write a factory test that both plan modes wire `External` with `codex_command` from config (agrouter-style path) and `Custom` with the script, and that `--codex` leaves `External` unset
- [ ] write a test that `Tool()` behavior is unchanged after the extraction, and runner tests for the two setters
- [ ] run `go test ./...` - must pass before task 6

### Task 6: PlanReviewPhase review loop

**Files:**
- Create: `pkg/processor/phase/plan_review_phase.go`, `pkg/processor/phase/plan_review_phase_test.go`

- [ ] define `PlanReviewPhase` with its opts struct (config, logger, planner executor, external and custom executors, policy, prompts, deps, repo reader) and `Run(ctx, PlanReviewRequest{PlanFile, Description})`
- [ ] resolve the mode from `cfg.AppConfig.PlanReviewer` (empty means ask); build the available reviewers (External reviewer only when the tool is not none and its executor exists; Same agent always; Skip review always); in ask mode call `AskQuestion` with the stable texts and log with `LogQuestion` and `LogAnswer`; for a preselected mode that is unavailable, log a warning and finish
- [ ] print a generic section header per round (`status.NewGenericSection`, for example `plan review round N`) and keep the phase holder on `PhasePlan`
- [ ] before each reviewer call take the repo snapshot; run the reviewer: codex and custom through the same adapter that `ExternalReviewPhase.runReviewTool` uses (extract or reuse it, so `CustomExecutor.Run` gets its prompt file), the same agent through `policy.Run` with the plan executor name; after it returns, take a second snapshot and compare
- [ ] on a changed repo: restore the plan file from the stored bytes if it changed (and drop it from the changed list), log a warning that names the changed paths and any HEAD move, discard the round's findings, finish the review without error; do not revert other files
- [ ] on context cancel return the error; pass pattern errors (rate limit, quota) through `HandlePatternMatchError` / `wrapExecutorError` first, as other phases do, so a limit waits or exits cleanly; on any other error, timeout or empty output log a warning and stop the review without error; apply the same rule to the revision session
- [ ] do not log the reviewer output again (executors already stream it through `OutputHandler`); parse findings; on no issues log it and finish; on unparseable output log the raw text and a warning and stop; otherwise print the numbered findings, ask the findings question (re-ask up to 2 times on an invalid answer, then treat as none), log the answer
- [ ] when findings are chosen run the planner revision session directly with `PlanRevisionPrompt`; require PLAN_READY in the result; on failure or timeout log a warning and finish; then repeat the loop (cap of 5 rounds; a preselected reviewer stops after a round that applies no findings)
- [ ] write tests with fake executors, moq mocks for the new interfaces and fake input collectors: ask then external then apply then Skip review; same agent; custom reviewer gets a prompt file path; preselected modes; unavailable external; `--codex` external skip; reviewer error and timeout do not fail the run; a limit pattern error is handled, not swallowed; reviewer output is not logged twice; a section header per round; plan file guard and restore; code file edited by the reviewer; reviewer commit moves HEAD; repo reader error falls back to plan-bytes-only; round cap; context cancel; stdout-only parsing with a doubled output; progress-file log calls for questions, answers, output and warnings
- [ ] run `go test ./pkg/processor/phase/...` - must pass before task 7

### Task 7: Runner integration for both modes

**Files:**
- Modify: `pkg/processor/runner.go`
- Modify tests: `pkg/processor/runner_test.go`

- [ ] build the review phase in the runner phase set (late-bound deps preserved through `phase.Deps`)
- [ ] `ModePlan`: record the start time, run plan creation, then find the plan with the finder and run the review phase with `cfg.PlanDescription`; if no plan file is found, log it and finish; keep `ErrUserRejectedPlan` handling
- [ ] `ModePlanReview`: require `cfg.PlanFile`, take the description from `plan.ExtractOverview`, set the plan phase on the phase holder, run only the review phase; no planner questions or drafts
- [ ] write runner tests: ModePlan runs the review after PLAN_READY and not after rejection; ModePlanReview runs the same phase and never calls plan creation; missing plan file error; the same loop code is hit from both modes
- [ ] run `go test ./pkg/processor/...` - must pass before task 8

### Task 8: main.go wiring for plan and plan-review modes

**Files:**
- Modify: `cmd/ralphex.windows/main.go`, `cmd/ralphex.windows/main_test.go`

- [ ] extract the "Continue with plan implementation?" tail of `runPlanMode` (abs path, worktree or branch creation, `executePlan` in `ModeFull`) into one helper used by both modes
- [ ] in `runPlanMode` call `r.SetPlanFinder(selector)` and `r.SetRepoState(gitSvc)`; pass `CodexEnabled: req.Config.CodexEnabled` and `ExternalReviewToolSet: o.externalReviewToolSet` in its `processor.Config` literal (today only `executePlan` sets them, `main.go:1060`, so `Tool()` would return `none` and External reviewer would always be hidden); completion message and continue question stay as they are
- [ ] add `runPlanReviewMode`: call `req.GitSvc.EnsureLocalGitignore()` first, as `runPlanMode` does (so `.ralphex/` is ignored); `selector.Select(ctx, o.PlanFile, false)` (existing fzf selector, no auto-plan-mode fallback), progress logger with `Mode: plan-review` and the plan file, startup banner, plan-model params in the run header, `processor.New` with `ModePlanReview` and the same `CodexEnabled` / `ExternalReviewToolSet` fields, `plan_model` spec as the planner model, input collector, `SetRepoState`; no branch, no worktree
- [ ] after the loop ask "Continue with plan implementation?"; no means exit; yes calls the shared helper; mark the progress log failed on a run error, as plan mode does
- [ ] route `ModePlanReview` in `run` next to `ModePlan` (before `selectAndExecutePlan`); update the startup info for the new mode
- [ ] write tests: mode routing, plan selection with an explicit file and with the selector, External reviewer is offered in both plan modes when codex is enabled, `EnsureLocalGitignore` is called in plan-review mode, no branch or worktree created in plan-review mode, continue yes and no, shared helper used by both modes
- [ ] run `go test ./cmd/...` - must pass before task 9

### Task 9: Verify acceptance criteria

- [ ] run `make test`; all tests pass
- [ ] run `make lint`; no issues
- [ ] run `make fmt`; no diff
- [ ] run `GOOS=windows GOARCH=amd64 go build ./...`
- [ ] verify coverage of the new code is 80% or more
- [ ] verify from the tests that the progress file keeps the review output, the findings, the questions, the answers and the guard warnings for both modes

### Task 10: Update documentation and delete the backlog item

**Files:**
- Modify: `README.md`, `llms.txt`, `CLAUDE.md`, `docs/custom-providers.md`
- Delete: `docs/backlog/plan-mode-has-no-external-plan-review.md`

- [ ] README: add a plan review subsection under Plan Creation for both entry points (flow, the three reviewers, the stable prompt texts, how to answer the findings prompt, the read-only guard and its known limit (a user edit or commit in another terminal during a review also discards that round's findings), the failure rule); add `--plan-review` and `--plan-reviewer` to the CLI flag table; add `plan_reviewer` to the config table; add `plan_review.txt` to the prompt file lists and the config directory tree; say that `--codex` skips the external reviewer and that direct plan execution runs no plan review
- [ ] llms.txt: add the `plan_review.txt` prompt, the `plan_reviewer` option, and the `--plan-review` and `--plan-reviewer` flags with examples (`ralphex --plan-review docs/plans/x.md --plan-reviewer=external`)
- [ ] CLAUDE.md: extend the Plan Creation Mode section with the review step, the standalone mode, the key files (`pkg/processor/phase/plan_review_phase.go`, `plan_review.go`, `plan_review_guard.go`, `pkg/config/defaults/prompts/plan_review.txt`), the design decisions above (including the repo guard and why the prompt alone is not enough), and the reuse of existing settings
- [ ] `docs/custom-providers.md`: say that custom review scripts must print findings on stdout for plan review
- [ ] delete the backlog file with `git rm`
- [ ] run the final test suite again - must pass

## Post-Completion
- Ask the user before running the toy end-to-end test (`./scripts/internal/prep-toy-test.sh`, then `ralphex --plan` and `ralphex --plan-review docs/plans/fix-issues.md`). It uses claude and codex credits.
- Follow-up outside this repo: the autopilot plugin in cc-stuff drops its own review-plan step and drives this native step (use `--plan-reviewer` or the stable prompt texts).
