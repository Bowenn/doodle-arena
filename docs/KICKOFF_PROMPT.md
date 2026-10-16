# Prompts for Claude Code

Copy these into Claude Code. Run them from the repo root.

## 1. First session: review before building

Paste this as the very first prompt:

```text
You're joining Doodle Arena, a real-time draw-and-guess game with a player marketplace.
It's a portfolio project for backend internship interviews, built by two humans (Bowen and
Will) who direct you. CLAUDE.md is already loaded; it explains the rules.

Before writing any code, read these files fully:
- docs/design-doc.md
- docs/roadmap.md
- docs/observability.md
- docs/keyword-coverage.md
- docs/setup.md
- everything in docs/adr/ and .claude/rules/

Then reply with:
1. A five-sentence summary of the system in your own words, so I can check you understood it.
2. Contradictions, gaps or risks you found in the docs, ordered by importance.
3. The tool and library versions you propose to pin for Stage 1 (Go, JDK, Spring Boot,
   Python, Node, Kafka, Postgres, Redis, MongoDB, SeaweedFS, grafana/otel-lgtm, Buf).
   Check that each version is current and maintained.
4. The questions you need answered before starting task 1.1, in particular for ADR 0001.

Don't create or change any files in this step.
```

After you've answered its questions and fixed any doc problems, start building:

```text
/milestone 1.1
```

## 2. Every later task

```text
/milestone 2.3
```

Add context after the task ID when it helps, for example:

```text
/milestone 2.5 We decided ADR 0002: the lobby returns each room's direct WebSocket URL.
Update the ADR to Accepted before you plan.
```

## 3. Starting Will's track before the game server exists

```text
/milestone 3.1 The game server isn't publishing events yet. Use scripts/fake-events.sh
and its fixtures for RoundFinished, and keep the consumer code identical to what real
events will need.
```

## 4. End-of-stage review

```text
Review Stage <n> against its exit criteria in docs/roadmap.md.
For each criterion: done or not done, and the evidence (test name, command output, or
dashboard). List anything that's only partly done, flaky tests, and gaps between the code
and docs/design-doc.md. Don't fix anything yet; give me the list first.
```

## 5. Interview drill

```text
Act as a backend interviewer at a payments company. Ask me 5 questions about
<the marketplace purchase flow | game-server failover | the webhook notifier>, one at a
time, based on the actual code in this repo. After each answer, tell me what was right,
what was missing, and what a strong answer would add. Don't show me the code until I've
answered.
```

## 6. Recording a number

```text
I just ran <the load test | the kill test>. Here's the output: <paste>.
Add the result to the evidence log in docs/roadmap.md with the date, how it was measured,
and a link to the dashboard. Then suggest one sentence I could put on my resume.
```
