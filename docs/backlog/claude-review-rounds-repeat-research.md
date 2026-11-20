---
worth: later
where: pkg/config/defaults/prompts/review_second.txt:17
added: 2026-11-19
---
# each claude review round repeats the research of the round before it

On a Windows run for ATH-2386 each Claude review round took about 3 hours, and round 1 did round 0's
research again: about 6 h 20 min of review for 1 hour of task work. A run on 2026-11-19 (svtlv
user-service step 3) showed the same shape: review 0 took 5 h, review 1 another 4 h 35 min. Round 1
gets the progress log (`{{PROGRESS_FILE}}`), but the log holds the agents' prose, not the facts they
established (what was checked, which areas are clean), so the new agents start over.

Options, cheapest first:
- a time cap per review round (`--session-timeout` already exists but also cuts task sessions);
- a way to skip the remaining Claude review rounds mid-run, like SIGQUIT does for the external review;
- a short "facts found" section that round N writes and round N+1 reads before it starts.

`later`: unknown whether a facts handoff really shortens round 1, or whether the time goes to the
agents' own exploration; one measured run with the handoff in the prompt would settle it.
