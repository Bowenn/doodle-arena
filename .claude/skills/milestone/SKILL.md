---
name: milestone
description: Plan and implement one Doodle Arena roadmap task (for example 2.3), with an approval step before any code is written.
argument-hint: "[task-id, e.g. 2.3]"
disable-model-invocation: true
---

Work on roadmap task $ARGUMENTS. Follow these steps for the whole task.

## 1. Understand

- Read the entry for task $ARGUMENTS in `docs/roadmap.md`, including its stage's exit criteria.
- Read the related sections of `docs/design-doc.md` and `docs/observability.md`, and every ADR in `docs/adr/` that the task touches.
- Check that the tasks it depends on are done. If one isn't, say so and stop.
- If the task depends on an ADR with status Proposed, present the options with their trade-offs and wait for a human decision.

## 2. Plan, then wait

Present a short plan:

- Files and services to create or change, and any change to `proto/`.
- Tests to add, including every test named in the exit criteria.
- Metrics, logs and spans to add, using the names in `docs/observability.md`.
- Risks, edge cases and open questions.

Then stop and wait for approval. Don't write code before the plan is approved.

## 3. Implement

- Work on the branch `stage-<n>/<task-id>-<short-name>`.
- Make small, working steps with one commit each (Conventional Commits, in English).
- After each step, run that service's tests. Before finishing, run `make test` and a `make up` smoke test.

## 4. Verify

- Check every exit criterion this task affects, and say how you checked it.
- Confirm the telemetry rules and the definition of done in `CLAUDE.md`.

## 5. Report

- Tick the task in `docs/roadmap.md`. If you measured numbers, add them to the evidence log there.
- Update the docs if behavior, contracts or commands changed.
- Append 3–5 talking points for this task to `docs/interview-notes.md`: the problem, the design choice, the alternative that was rejected and why, and how the design fails.
- End with a summary: what changed, how a human can verify it, and the decisions and trade-offs in plain English.
