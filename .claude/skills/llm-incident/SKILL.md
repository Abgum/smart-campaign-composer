---
name: llm-incident
description: Use when a live Claude turn in smart-campaign-composer fails or behaves unexpectedly, such as repair_exhausted, max_tokens, a wrong target, or a missing or incorrect saved draft.
---

# Live-model incident

**Rule:** Do not raise a retry, iteration, timeout, or `max_tokens` limit, or change the prompt, before proving the cause.

## Steps

1. **Collect facts.**
   - Record the turn ID, chat ID, user message, and error shown in the UI.
   - Record the model and effort (`LLM_MODEL`, `LLM_EFFORT`).
2. **Inspect records in read-only mode.**
   - Run `docker compose exec -T postgres psql -U scc -d scc`; make `SET default_transaction_read_only = on;` the first command.
   - Inspect `turns`: status, outcome, error_code, route.
   - Inspect `llm_calls`: kind, iteration, stop_reason, error_code, token counts, latency.
   - Inspect `messages` and the relevant `campaign_versions`.
   - Note: `llm_calls` does not store raw requests or responses, so it cannot show exactly what the model received.
3. **State a hypothesis.** Write one sentence: “X causes Y.” Identify an observation that would confirm or refute it.
4. **Reproduce in a copy.** Do not disturb the user's running application.
   1. Copy the database with `pg_dump`.
   2. Restore it in a separate Compose project (for example, `-p scc-flaky`, with a different image tag and ports).
   3. Reconstruct the state at the start of the turn in that copy.
   4. Replay the turn several times with the real runner and an LLM wrapper that records requests and responses. Report token usage.
5. **Show the cause.** Point to the recorded request or response that confirms the hypothesis. If it does not, return to step 3.
6. **Fix it.** Follow `fix-with-red-test`: write a failing test that exposes the cause, then make the fix.
7. **Replay in the copy.** Run the same scenario before and after the fix and report the outcomes side by side.
8. **Update documentation.** If behavior changed, update `DECISIONS.md` and the “Scope and limitations” section of `README.md`.
