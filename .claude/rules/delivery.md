---
paths:
  - "backend/internal/delivery/**"
  - "backend/internal/provider/**"
  - "backend/internal/mockprovider/**"
  - "backend/internal/campaign/approve.go"
  - "backend/internal/store/migrations/**"
  - "backend/internal/seed/**"
---

# Delivery rules

Purpose: handle duplicate content and uncertain outcomes safely. The database blocks duplicate content; a single acceptance across repeated provider calls depends on the provider's idempotency support. Answer each question below when changing delivery code.

## Questions for every change

- **Is the idempotency key preserved?** `Idempotency-Key` is the delivery ID and stays the same for every attempt. Do not generate a new key per attempt.
- **Does an uncertain outcome remain uncertain?** A transport error before `GotConn` means the request was definitely not sent. A transport error after connection establishment, including a write error, is ambiguous. Classify HTTP responses separately: 2xx accepted, 408/429 retryable rejection, other 4xx permanent rejection, 3xx/5xx ambiguous. After an ambiguous attempt, a delivery may end as `sent` or `unknown`, never `failed` or `skipped_no_consent`.
- **Does every provider call obtain a rate grant?** Each call obtains permission through `grant_call` first. A grant is usable only if the call starts within 50 ms of the request for the grant. Discard late grants and request another. Do not add an in-memory limiter.
- **Is consent checked before the call?** Reread the recipient's consent and contact details at approval and immediately before each call. Do not call the provider without consent.
- **Are result writes fenced?** Each write requires `status = 'in_flight' AND attempts = <value at claim time>`. If the condition fails, write nothing (`ErrLeaseLost`).
- **Are unstarted calls released without counting an attempt?** Use `Release` when a pause, busy channel, short lease, or shutdown prevents a call from starting.
- **Is the timing budget preserved?** Lease > CallBudget > provider timeout (30 > 15 > 10 seconds). Start a call only when at least CallBudget remains on the lease.
- **Is recording attempted for calls that started?** Worker shutdown does not cancel an in-progress call (`context.WithoutCancel`). Record its result with a separate, bounded context. A database error or lost lease can still prevent the write; do not report it as successful.

## Additional constraints

- Workers claim no more deliveries than they have free execution slots.
- Enforce duplicate prevention with database constraints (a unique index and `ON CONFLICT DO NOTHING`), not an application-level check followed by an insert.
- `unknown` retains the deduplication slot; only `failed` and `skipped_no_consent` release it.
- Seed and demo code do not write delivery outcomes directly. Historical messages also pass through the real worker and mock provider.
- The mock provider must be a reliable observer: persist acceptances in an fsynced, append-only log. For a previously accepted key, replay the first acceptance before applying a scripted 429, 400, or 503 response.
- Make schema changes in a new migration. Do not edit an applied migration.
- When behavior changes, update both the “Scope and limitations” section of `README.md` and `DECISIONS.md`.
