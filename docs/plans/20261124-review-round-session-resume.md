# Resume a retried review round instead of repeating its research

## Overview

A review round that stops on a rate limit (with `--wait`), a transient API error, or a session timeout runs again in a new session today. The new session repeats all research from the first step.

This change gives each review round one session id, kept in process memory only. A retry of the same round resumes that session with a short "continue" prompt. Each new round starts a clean session. Wrapper scripts keep today's behavior.

Scope (from the backlog item `docs/backlog/claude-review-rounds-repeat-research.md`):

- Rounds: review 0 (`First`) and every iteration of `Loop` (pre-external and post-external).
- Claude: attempt 1 runs `--session-id <uuid>` with the full prompt. A retry runs `--resume <uuid>` with the continue prompt.
- Codex: attempt 1 runs `codex exec`. ralphex reads the session id from the stderr banner. A retry runs `codex exec ... resume <id> -`.
- No state file. Nothing new in the progress log. A restarted ralphex starts from the beginning.
- If the resume fails, fall back to a new session with the full prompt. Resume a timed-out session only once, then start a new one.
- Resume only for the real `claude` and `codex` CLIs, plus the `agrouter` launcher for claude (see Task 1). Other `claude_command` wrapper scripts never resume. Codex through `codex-agrouter.sh` never resumes. Do not pass `--no-session-persistence`.

## Context

- Files involved:
  - `pkg/executor/executor.go` (ClaudeExecutor.Run, args building, `stripFlag`, `Result`)
  - `pkg/executor/codex.go` (CodexExecutor.Run, `extractSessionID`, `processStderr`)
  - `pkg/processor/phase/review.go` (`First`, `Loop`, `run`)
  - `pkg/processor/phase/phase.go` (consumer-side interfaces, `Executor`, `Policy`)
  - `pkg/processor/execution_policy.go` (`retryPolicy.Run`: wait retry, transient retry, session timeout)
  - `pkg/processor/phase/mocks/` (moq mocks), `CLAUDE.md`, `llms.txt`, `README.md`, `docs/custom-providers.md`
  - `docs/backlog/claude-review-rounds-repeat-research.md`, `docs/backlog/README.md`
- Design decision: keep `Executor.Run(ctx, prompt)` and the `Policy` interface unchanged. A new per-round helper in `pkg/processor/phase` has a `Run(ctx, prompt) executor.Result` method with the same signature as `exec.Run`. Passing `round.Run` to the policy makes every retry resume with no change to the policy or to existing mocks. Only the rate-limit wait retry calls `run` again inside `Policy.Run`. A transient retry pattern and a session timeout come back from `Policy.Run` as a `TimedOut` result, and the phase loop then starts the next iteration. That next iteration reuses the same round object, so it also resumes. The helper swaps in the continue prompt and the resume flags on every call after the first.
- Optional capability interface (defined in `pkg/processor/phase`, consumer side): `SessionExecutor` with `Executor` plus `CanResume() bool`, `NewSessionID() string`, and `RunSession(ctx, prompt string, s executor.SessionSpec) executor.Result`. Executors that do not implement it (mocks, wrappers) behave as today.
- agrouter in this repository: `claude_command = agrouter` and `claude_args = exec claude --doc=... -- --dangerously-skip-permissions --output-format stream-json --verbose`. ralphex appends `--model`, `--effort` and `--print` after the `--`, so agrouter forwards them to claude. Session flags are appended in the same place and follow the same path.
- Related patterns: table-driven tests with testify, moq mocks in `mocks/`, lowercase comments, one `_test.go` per source file, no new dependencies (UUID v4 comes from `crypto/rand`).
- Dependencies: none new.

## Development Approach

- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- **CRITICAL: every task MUST include new/updated tests**
- **CRITICAL: all tests must pass before starting next task**
- Do not edit `CHANGELOG`. Do not touch completed plans.

## Implementation Steps

### Task 1: Claude executor session flags

**Files:**
- Modify: `pkg/executor/executor.go`
- Modify: `pkg/executor/executor_test.go`

- [x] add `SessionSpec{ID string; Resume bool}` and `Result.SessionID string` to the executor package
- [x] add `(*ClaudeExecutor).RunSession(ctx, prompt, spec)`; `Run` calls it with an empty spec, so the default argv stays the same
- [x] with an ID and no Resume, append `--session-id <id>`. With Resume, append `--resume <id>`. Strip any `--session-id` / `--resume` already in configured args with `stripFlag`. Append them next to `--model`/`--effort`, before `--print`, so they land after the `--` separator when a launcher is used
- [x] add `CanResume()`. It is true when configured args do not contain `--no-session-persistence` and one of these holds: (a) the command is empty or its base name (minus `.exe`/`.cmd`/`.bat`) is `claude`; (b) the base name is `agrouter` and the configured args start with `exec claude` (the first two fields). Every other command returns false. Never add `--no-session-persistence` ourselves
- [x] add `NewSessionID()`: random UUID v4 from `crypto/rand`; return the ID in `Result.SessionID`
- [x] tests (table-driven): argv for new / resume / empty spec, flag de-duplication, UUID format and uniqueness
- [x] tests (table-driven): `CanResume` for `claude`, `claude.cmd`, `/path/claude`, `codex-as-claude.sh`, `agrouter` with `exec claude ...`, `agrouter` with other args (`exec codex`, empty args), `/opt/bin/agrouter` with `exec claude`, and args containing `--no-session-persistence`
- [x] test for the agrouter argv: command `agrouter`, args `exec claude --doc=CLAUDE.md --doc=llms.txt -- --dangerously-skip-permissions --output-format stream-json --verbose` with a model, effort and a resume spec. Assert that `--resume <id>` and `--print` come after the `--` separator, and that `exec claude --doc=... --` stays unchanged before it
- [x] run project test suite - must pass before task 2

### Task 2: Codex executor resume

**Files:**
- Modify: `pkg/executor/codex.go`
- Modify: `pkg/executor/codex_test.go`

- [x] record the session id from `processStderr` in `stderrResult` (the existing `sessionIDCh` flow stays) and set `Result.SessionID` on every return path, including idle-timeout and error returns
- [x] add `(*CodexExecutor).RunSession(ctx, prompt, spec)`; `Run` calls it with an empty spec
- [x] with Resume, build `codex exec <overrides> --sandbox <mode> -c ... resume <id> -`, with the prompt still sent through stdin. Keep the model, effort, sandbox and bypass flags the same as a fresh run
- [x] add `CanResume()` (command empty or base name `codex` or `codex.exe`, so an explicit path such as `C:\Users\me\.codex\bin\codex.exe` works; split the path on both `/` and `\`; a wrapper such as `codex-agrouter.sh` or `codex-as-claude.sh` returns false) and `NewSessionID()` (returns `""`, because codex assigns the id)
- [x] rollout reader on resume: codex appends to the existing rollout file of the session, and `tailRolloutFile` starts at byte zero, so it would replay old output into the progress log. Before a resume run, find the rollout file for the id and record its size. Start `tailRolloutFile` at that offset. A fresh run still starts at zero. If the file is not found before the run, start at zero
- [x] tests: argv for fresh and resume runs, `Result.SessionID` filled from a stubbed stderr banner on success, error and idle-timeout paths, `CanResume` cases including `codex`, `/usr/local/bin/codex`, `C:\...\codex.exe` and `.ralphex/scripts/codex-agrouter.sh` returning false
- [x] test: a rollout file with old records and a resume run with new records appended forwards only the new records to the output handler
- [x] run project test suite - must pass before task 3

### Task 3: Per-round session helper

**Files:**
- Create: `pkg/processor/phase/round.go`, `pkg/processor/phase/round_test.go`
- Modify: `pkg/processor/phase/phase.go` (add `SessionExecutor` interface and `go:generate moq` line)
- Create: `pkg/processor/phase/mocks/session_executor.go` (generated with moq)

- [x] add `SessionExecutor` interface in `phase.go` and generate its mock
- [x] add `reviewRound` with `newReviewRound(parent context.Context, exec Executor, sessionTimeout time.Duration)` and `Run(ctx, prompt) executor.Result`. The round takes no logger, because it writes nothing to the progress log. `parent` is the phase context and `sessionTimeout` is the configured session timeout (use the same value and the same claude/codex gating as `retryPolicy.runWithSessionTimeout`; find the smallest way to hand it to the phase, for example through `phase.Deps` or an accessor) If `exec` is not a `SessionExecutor` or `CanResume()` is false, `Run` calls `exec.Run` with the full prompt (current behavior)
- [x] first call: take the id from `NewSessionID()`, run with the full prompt and `SessionSpec{ID}`, then keep `result.SessionID` when the executor assigns its own id (codex). If no id is known, later calls use the full prompt in a new session
- [x] later calls: run `SessionSpec{ID, Resume: true}` with a short Go-side continue prompt (constant `resumeRoundPrompt`: continue where you stopped, do not repeat finished research or agent runs, and finish the review by following the completion rules of the original instructions exactly; for example, both default review prompts require a plain-text summary with no signal after a fix commit, so the prompt must not ask for a signal in every case)
- [x] resume failure fallback: if a resume attempt returns a plain error (not a pattern error, not context cancellation) and its output or diagnostic text contains a specific missing-session message, take a new id and run the full prompt in a new session in the same call. Match these messages as case-insensitive substrings: `No conversation found` (claude) and `no rollout found for thread id` (codex, on stderr, so read the codex stderr tail or diagnostic text as well as stdout). Do not treat empty output as a missing session, because empty output also occurs on unrelated failures. Log nothing: the fallback must add no entry to the progress log
- [x] fallback timeout: the fallback session must get its own full session timeout, not the time left from the failed resume. Run it with a context derived from `parent` (so user abort still cancels it) with a new `context.WithTimeout(parent, sessionTimeout)` when the timeout applies, instead of the `ctx` passed to `Run`. If the fallback times out, return that result as `TimedOut` and apply the `noteTimedOut` rules to the new session
- [x] `noteTimedOut()`: the first timeout keeps the session so the next call resumes; a second timeout in the same session drops it, so the next call starts a new session with the full prompt and a new id
- [x] tests in `round_test.go` (table-driven, moq `SessionExecutor` mock), calling `round.Run` directly: first call args, resume args and prompt, wrapper/non-session executor passthrough, codex id capture, missing codex id, fallback on both specific session-not-found messages, no fallback on empty output, pattern error or cancel, fallback session runs under a fresh timeout (the first context is already expired), no log output on fallback, resume-once-then-new after timeouts, and the continue prompt text keeps the original completion rules
- [x] run project test suite - must pass before task 4

### Task 4: Wire rounds into the review phase

**Files:**
- Modify: `pkg/processor/phase/review.go`
- Modify: `pkg/processor/phase/review_test.go`
- Modify: `pkg/processor/phase/round_test.go`
- Modify: `pkg/processor/execution_policy_test.go`

- [ ] `First`: create one round and call `p.policy.Run(ctx, round.Run, prompt, execName)`. A rate-limit wait retry inside the policy now resumes. `First` still does not retry after a session timeout (separate decision in the backlog item)
- [ ] `Loop`: keep one round per iteration. `Loop` handles `TimedOut` before it checks for a commit, so a timed-out round may already have ended with a commit. After a `TimedOut` result, compare `HEAD` with the value taken at the start of the round. If `HEAD` moved, the round ended: drop the round and do not resume. If `HEAD` did not move, call `round.noteTimedOut()` and keep the same round for the retry iteration. Drop the round when the iteration ends with a signal, a commit, or no change. The next iteration (and the post-external `Loop` call) starts a new round with a new id
- [ ] round-plus-retry tests live in `pkg/processor/phase` (`round_test.go` and `review_test.go`), because `reviewRound` is unexported. Use a scripted `Policy` (moq mock or small fake). Simulate each retry kind the way the real policy behaves: for the wait retry, the fake `Run` calls the passed `run` function a second time inside the same `Policy.Run`; for a transient retry pattern and for a session timeout, the fake `Run` returns a `TimedOut` result after one call, and the next phase iteration calls the policy again with the same round. Assert the first call uses `--session-id` semantics with the full prompt, the second uses resume with the continue prompt, a timeout is resumed once, and the third try starts a new session with a new id. Add a `Loop` case where the timed-out iteration moved `HEAD`: the next iteration must start a new session and not resume
- [ ] review phase tests in `review_test.go`: a wrapper-style executor (no `SessionExecutor`) gets the full prompt every time; ids are not reused across iterations or across the two `Loop` calls; `First` and `Loop` pass `round.Run` to the policy
- [ ] `execution_policy_test.go`: keep only checks that need the real `retryPolicy` through the exported phase API, for example a limit pattern with a wait duration followed by success, using a real `ReviewPhase` and a scripted `SessionExecutor`, asserting the retry calls resume with the continue prompt. Do not test the unexported `reviewRound` there
- [ ] run `make test`; all tests must pass before Task 5

### Task 5: Verify acceptance criteria

- [ ] run full test suite: `make test`
- [ ] run linter: `make lint` and format: `make fmt`
- [ ] cross-compile check: `GOOS=windows GOARCH=amd64 go build ./...`
- [ ] verify test coverage of new code meets 80%+

### Task 6: Update documentation and remove the backlog item

**Files:**
- Modify: `CLAUDE.md`, `llms.txt`, `README.md`, `docs/custom-providers.md` (only if it describes retry or wrapper limits)
- Delete: `docs/backlog/claude-review-rounds-repeat-research.md`
- Modify: `docs/backlog/README.md`

- [ ] `CLAUDE.md`: add a Key Patterns bullet for review-round session resume (in-memory id per round, `round.Run` wraps the executor so the policy is unchanged, resume once after a timeout, fallback to a new session, real `claude`/`codex` and `agrouter ... exec claude` only, `codex-agrouter.sh` and `*-as-claude` wrappers never resume, no state file)
- [ ] `llms.txt` and `README.md`: update the transient retry, rate limit retry, and session timeout text to say that same-process review retries resume the round session for the real claude and codex CLIs (and agrouter running claude), and that other wrappers and restarts start fresh
- [ ] `docs/custom-providers.md`: note that `*-as-claude` wrappers never resume
- [ ] delete the backlog item and remove its entry (item 10) from `docs/backlog/README.md`
- [ ] run `make test` and `make lint` once more

## Post-Completion

Manual checks (ask the user first, because they use claude and codex credits):

- Build the new binary (`make build`). Prepare the toy repository with `./scripts/internal/prep-toy-test.sh` (it only prepares the repository, it does not run ralphex). In `/tmp/ralphex-test`, create a feature branch with a commit, then run `.bin/ralphex --review --session-timeout=<short value>` with the new binary so a timeout happens in the review loop. Check in the progress file that the retry resumes the same session without repeating research, and that the progress file has no new resume or fallback entries.
- In an agterm session, capture one real `agrouter exec claude ... -- --print --session-id <uuid>` run, then a `--resume <uuid>` run with the same uuid. Confirm agrouter passes `--session-id` and `--resume` through to claude and that the resumed run keeps the earlier context.
- In an agterm session, capture one real `claude --print --resume <unknown-uuid>` run and one `codex exec resume` run. Confirm the "session not found" text and the `resume <id> -` argument order.
- Check what a resumed claude transcript holds when the stop came while parallel agents ran.
