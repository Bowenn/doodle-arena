# ADR 0011: Moderation retries, then falls back to the review queue

- Status: Accepted
- Date: 2026-10-17
- Deciders: Bowen

## Context

Every drawing is moderated by the AI service before it can be minted (design doc 4.5, task 3.3). The service can be slow, down or overloaded, and later (Stage 7) a real model may be unsure. A drawing must never be approved without a decision, never lost, and never stuck without anyone noticing. M3 asks for a test: when the AI stub is slow or failing, the call is retried, and after the last attempt the drawing lands in the review queue.

Related choices made at the same time: what assets sends (the thumbnail's PNG bytes in the body, so the AI service needs no storage access), what the AI may answer (approve, reject, or review when unsure), and how the stub is controlled (environment variables plus `PUT /internal/moderation-mode` on the internal port 9091).

## Options

### Option A: retry with backoff and jitter, then the review queue

Each call has a 3-second timeout. Timeouts, connection errors, `429` and `5xx` are retried with exponential backoff and full jitter; any other `4xx`, or an unreadable answer, goes to the queue at once. The budget (4 calls) is counted on the drawing's row (`moderation_attempts`), so it holds across task redeliveries and the sweeper. After the last failed call the drawing waits in the Django admin's review queue with the reason.

- Pros: simple and visible; an AI outage slows moderation down and never loses or auto-approves a drawing; the queue is the same place a real model's "unsure" answers go.
- Cons: during a long outage every drawing spends its 4 calls and the human queue fills up.

### Option B: the same, plus a circuit breaker

After N consecutive failures, skip calls for a cool-down and send drawings straight to the queue (or let them wait for the sweeper).

- Pros: saves load and latency on a service that is clearly down.
- Cons: shared breaker state across pool processes (Redis), more states to test, and it fills the queue faster, not slower, unless drawings wait instead.

## Decision

Option A, decided by Bowen on 2026-10-17 (task 3.3). It meets the M3 criterion with the least state, and the queue is a fallback people already use.

## Consequences

- Only one task calls the AI service about a drawing at a time: claiming it (`moderation_claimed_until`, 60 s) and counting the attempt are one conditional UPDATE, so a redelivered task can't spend the budget twice.
- The stub stops working when its caller hangs up (it checks for a disconnect while it waits), so a timed-out call doesn't keep burning its capacity.
- A late AI answer can't overrule the queue: the AI decides only from `processing`, a moderator only from `review` (the guarded `decide()`).
- Revisit with a breaker if the AI service becomes a paid or rate-limited dependency (Stage 7).
