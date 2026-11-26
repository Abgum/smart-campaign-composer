# Evidence and experiment rules

- Do not increase retry counts, iteration limits, timeouts, or `max_tokens` before proving the cause of a failure.
- State a one-sentence hypothesis first (“X causes Y”), then collect an observation that can confirm or refute it.
- Query the user's database only in read-only mode: set `PGOPTIONS='-c default_transaction_read_only=on'`, or run `SET default_transaction_read_only = on;` as the first command in a psql session.
- Do not disturb the running application for experiments. Use `pg_dump` to copy the database into a separate Compose project (for example, `-p scc-flaky`, with a different image tag and ports), then reproduce the issue there.
- Demonstrate a fix against the actual behavior: replay the same scenario before and after the fix and report the results side by side.
- If a step fails unexpectedly or its result is unclear, stop and report where you stopped. Do not assume it passed.
