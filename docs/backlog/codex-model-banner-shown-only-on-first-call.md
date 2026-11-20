---
worth: yes
where: pkg/executor/codex.go:245
added: 2026-11-19
---
# codex model and effort are shown only on the first codex call of a run

`headerEmitted` lets `shouldDisplay` pass codex's `model:` / `reasoning effort:` header lines through
once per executor, so every later codex call in the run (external review iterations 2+, codex
executor tasks) prints no model at all. With `codex_command` routed through agrouter the model and
effort can change per call, so the first banner does not describe the later ones. The same filter also
hides agrouter's `agrouter: selection: {...}` line, which agrouter writes to codex's stderr before it
starts the child.

Seen in an autopilot run on 2026-11-19: codex iterations 2-8 of the external review reported nothing,
and the chat showed "runner default model" for them. agrouter's own log
(`<log_dir>/agrouter.log`) had a selection for every call (gpt-6.2-sol, xhigh). autopilot now reads that
log as a workaround, which only works when agrouter is the codex_command.

Fix: show the three header config lines on every codex call (or on every call whose resolved model or
effort differs from the previous one), and pass through stderr lines that start with `agrouter: `
the way the claude executor's output already does. Touches `shouldDisplay`/`processStderr` and their
tests in `codex_test.go`; the "show once" behavior is a deliberate upstream choice, so the per-call form
may need a flag.
