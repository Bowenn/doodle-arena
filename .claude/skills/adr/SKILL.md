---
name: adr
description: Create a new architecture decision record (ADR) in docs/adr/ from the template.
argument-hint: "[short title]"
disable-model-invocation: true
---

Create an ADR titled "$ARGUMENTS".

1. Find the highest number in `docs/adr/` and use the next one, zero-padded to four digits.
2. Copy `docs/adr/0000-template.md` to `docs/adr/<number>-<kebab-case-title>.md`.
3. Fill in Context, Options (at least two, each with pros and cons) and Consequences from the current conversation.
4. Set Status to Proposed unless a human has already decided. If they have, set it to Accepted and fill in Decision with their reasoning.
5. If the new ADR replaces an earlier one, set the earlier one's status to "Superseded by <number>".
6. When the ADR is Accepted, add a line for it to the decision log in `docs/design-doc.md`.
