---
paths:
  - "backend/internal/chat/**"
  - "backend/internal/llm/**"
  - "backend/internal/segment/**"
  - "backend/internal/campaign/**"
---

# LLM boundary

## Authority

- The model proposes drafts only. It cannot approve, send, or write SQL. Do not add a tool that grants any of those capabilities.
- Enforce every data-integrity rule in backend code; a prompt instruction alone is insufficient. If breaking a rule would corrupt data, enforce it in code. If it would only reduce output quality, it may remain in the prompt.
- Do not use regex or text parsing to make natural-language decisions, even as a consistency check. The only exception is the `#N` safeguard: it can trigger a more cautious clarification, never authorize a write.

## Ambiguity

- Ask instead of guessing about an ambiguous target, ambiguous place name (save nothing in a turn with `ambiguous_place`), or language group not yet presented to the user.
- Do not silently drop targets above the limit; report them as `overflow`.
- If the model omits a field, preserve its existing value (for example, `ab_test`). Removal is a separate, explicit operation (`remove_ab_test`).

## Model output

- Check `stop_reason` first. Use nothing from a response ending in `max_tokens` or `refusal`.
- Always parse tool input with `decodeStrict`; reject unknown fields and trailing data.
- Return validation errors to the model as `is_error` tool results with `{path, code, message}`.
- Audience counts come from tool results, never from the model's estimates.

## Strict tool schemas

- Fields the model must not omit should be required and nullable (for example, `campaign_id` and `base_version`).
- Derive property order from the `required` list using `inOrder` / `orderedObject`. Go maps are serialized with alphabetically sorted keys; do not rely on their order.
- Keep the total number of optional parameters in strict tools at or below 24 (`TestStrictTools_OptionalParametersWithinClaudeLimit`).
- After changing a schema, run the test that inspects the raw request sent to Claude (`TestStream_StrictSchemaInRequiredOrder`).

## Context

- Keep the system prompt identical across turns for prompt caching. Put turn-specific data in `<chat_state>`.
- Send only aggregate counts to the model, never recipients' personal data (names, phone numbers, or email addresses).
- Do not present mock output as genuine interpretation; label canned drafts in mock mode.

## Segment SQL

- `segment/compile.go` generates SQL for model-proposed audience rules: column expressions are fixed in code; rule values and the current time are bind parameters.
- Audience queries use `statement_timeout`: 3 seconds for a count, 15 seconds during approval.
- Validation rejects the naive abandoned-cart rule (`cart_add` plus negated `purchase`). Use `not_followed_by` instead.
