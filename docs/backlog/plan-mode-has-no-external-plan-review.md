---
worth: yes
where: pkg/processor/phase/plan_creation.go:157
added: 2026-11-16
---
# plan mode has no external plan review

`--plan` runs only QUESTION and PLAN_DRAFT rounds: `handleDraft` sends the draft straight to
`AskDraftReview`, and the finished plan file gets no review by another model. The execution pipeline has
one (`external_review_tool` = codex/custom), plan creation does not, so callers bolt it on from outside.
The autopilot plugin (cc-stuff) does that today: it runs Codex read-only on the plan and asks the user which
findings to apply, in the way `/planning:make` offers "Auto review" (plan-review agent or external
reviewer) after the plan file is written.

Wanted: an optional review step after the plan file is written (or before `AskDraftReview`), using the
configured `external_review_tool` / `custom_review_script` with a plan-review prompt (`plan_review.txt`:
wrong assumptions about the code, missing steps, over-engineering, task order, CLAUDE.md conventions,
`CRITICAL|MAJOR|MINOR: <section> - <text>` or `NO ISSUES FOUND`). Findings go to the input collector as a
pick-which-to-apply prompt, and the chosen ones become revision feedback. Read only the reviewer's stdout:
codex echoes the whole prompt on stderr, which doubles every finding when both streams are merged. Once it
exists, autopilot can drop its own `review-plan` step and drive the native one.
