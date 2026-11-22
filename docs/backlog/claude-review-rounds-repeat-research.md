---
worth: yes
where: pkg/processor/execution_policy.go:29
added: 2026-11-19
---
# a retried claude review round repeats its whole research

On a Windows run for ATH-2386 each Claude review round took about 3 hours: about 6 h 20 min of review
for 1 hour of task work. A run on 2026-11-19 (svtlv user-service step 3) showed the same shape: review 0
took 5 h, review 1 another 4 h 35 min. When such a round stops on a rate limit, a transient API error or a
session timeout and runs again, the retry starts a new session and repeats the research from the first
step, so hours of finished agent work are lost.

Every retry inside one process makes a new session today:

| Cause | Retry path | Where |
|---|---|---|
| rate limit with `--wait` | waits, then the same prompt in a new session | `execution_policy.go:53` |
| transient error (`claude_retry_patterns`) | handled as a timeout | `execution_policy.go:37` |
| session timeout in the review loop | next iteration, new session | `phase/review.go:87` |

Decided scope (2026-11-22):
- Reuse applies only to retries of the same round inside the same ralphex process. Keep the session
  id in memory only; write no state file.
- Each round still starts with a clean session and its own investigation. Clear the id when the round
  ends with a signal or a commit.
- A stopped and restarted ralphex starts from the beginning, as today.

Direction: ralphex makes a UUID per round and passes `claude --session-id <uuid>`. A retry of that round
passes `--resume <uuid>` with a short "continue where you stopped" prompt instead of the full prompt.
Codex has `codex exec resume <id>`, and ralphex already reads the codex session id (`executor/codex.go:454`).
If the resume fails (session not found), fall back to a new session with the full prompt. Resume a
timed-out session only once, because a session that hung can hang again.

Open points for the plan:
- Wrappers (`claude_command` set to a `*-as-claude` script) cannot resume; a "continue" prompt without
  context would break them. Decide how to enable resume only for real `claude` and codex.
- `Executor.Run(ctx, prompt)` takes no per-call options, so the executor interface and its mocks change.
- Not verified: what a resumed transcript holds when the stop came while parallel agents ran. Finished
  agents' results may or may not be there. Check with one captured run in agterm before relying on it.
- Review 0 does not retry after a session timeout at all (`phase/review.go:135`); it logs a warning and
  goes to the loop. Whether it should retry is a separate decision.
- After a long `--wait` the prompt cache is gone, so the first resumed call reads the full context again.
