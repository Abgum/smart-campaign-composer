# CLAUDE.md

## Git

Unless I explicitly ask, do not run `git add`, `git commit`, or `git push`, and do not rewrite Git history. Leave changes in the working tree and show me the diff and test results.

## Project

Smart Campaign Composer lets a marketer describe a campaign in natural language. Claude proposes an audience rule and SMS or email drafts. The backend validates the proposal and saves a version. Approval is a separate API operation. A PostgreSQL queue and workers send messages through the mock provider.

- Backend: Go 1.26 in `backend/`. One binary serves different roles through its first argument: `api | worker | mockprovider | migrate | seed | history | probe`.
- Frontend: Vue 3 and TypeScript in `frontend/`.
- Database: PostgreSQL 17. Tables are defined in `backend/internal/store/migrations/00001_init.sql` and subsequent migrations.
- Design rationale: `DECISIONS.md`. Setup and limitations: `README.md`. Read the relevant rationale in `DECISIONS.md` before changing a design decision.

## Commands

| Task | Command |
| --- | --- |
| Start the application | `make up` (UI: http://localhost:8080) |
| Load demo history | `make demo-data` |
| Run all tests | `make test` (in Docker; local Go, Node, and an API key are not required) |
| Run backend tests | `make test-backend` |
| Run one database test | `make volumes`, then `docker compose -f docker-compose.test.yml run --rm backend-test go test -race -count=1 -v -run '^TestName$' ./internal/package/`, then `docker compose -f docker-compose.test.yml down` |
| Run one test without a database | `cd backend && go test -count=1 -v -run '^TestName$' ./internal/package/` |
| Check formatting and static analysis | `make lint` |
| Verify GeoNames data | `make verify-geonames GEONAMES_DUMP=local-data/geonames/cities15000.txt` |

## Design invariants

These are deliberate decisions. Ask me before changing one.

- **The model proposes drafts; the backend validates them.** The model has no tool to approve, send, or execute SQL. Campaign approval is a separate API request.
- **The LLM is absent from approval and delivery.** Those paths are deterministic backend code.
- **The segment compiler generates SQL for model-proposed audience rules.** Column expressions are fixed in code; rule values are bind parameters.
- **Duplicate content is blocked per recipient and channel.** `ux_deliveries_message` prevents a second delivery holding the same rendered content; an `unknown` delivery keeps its slot. A single acceptance across provider retries depends on the provider honoring idempotency keys.
- **A transport error after connection establishment is ambiguous** (`httptrace.GotConn` is the boundary). HTTP responses are classified separately: 2xx accepted, 408/429 retryable rejection, other 4xx permanent rejection, and 3xx/5xx ambiguous. Once an attempt is ambiguous, that uncertainty is retained.
- **`Idempotency-Key` is the delivery ID** and stays the same across attempts.
- **Rate limits and 429 pauses live in PostgreSQL** and are shared across workers and restarts. They use database time.
- **A revision of an existing draft has a base version.** A new campaign has a `null` base version. A stale revision returns `stale_version`. Content that the user has not seen cannot be approved.
- **The router model interprets natural-language intent and targets.** The backend never grants write authority through regex or text parsing. Its only text-based exception is a cautious check that asks for clarification when a campaign is ambiguously mentioned as `#N`.

## Workflow

1. **Plan first when requested.** If I say “do not write code yet,” provide only a plan and wait for my approval before coding.
2. **Work on one slice at a time.** If I say “do not proceed to the next step,” stop after the current slice.
3. **Start every code change with a failing test.** Follow the `fix-with-red-test` skill.
4. **Before claiming completion,** check the applicable `.claude/rules/` files. Report changed files and the red and green test output.
5. **For live-model failures,** follow the `llm-incident` skill. Prove the cause before raising a limit.
6. **Other people or agents may share this working tree.** Reread files before editing and do not revert their changes.
7. **Do not disturb my running application** on ports 8080, 8090, or 9091. Run experiments in a separate Compose project with different image tags and ports.
8. **Finish the core first.** Add bonus features only after the core flow passes end-to-end checks.

## Evidence

- Do not present an unverified claim as verified. Say “not verified” when appropriate.
- Support any claim using “guarantee” or “never” with a test or measurement.
- Do not raise a timeout, sleep, retry count, or iteration limit just to make a test pass. Find the cause first.
- Report the commands you ran and their actual output. SKIP is not PASS.
- Do not document behavior that the code does not implement, or claim a flow was tested with the live Claude API when it was not.

## Language

- Respond to me in Turkish. Write code, code comments, and commit messages in English.
- Keep technical surfaces in English: system prompts, tool and schema descriptions, notes read only by the model, and URLs.
- Human-facing text must originate in Turkish and be translatable: UI strings use `t()` or `tk()` with translations in `en.ts`; API error messages use `internal/i18n`.
- Refer to languages and countries by name when addressing users (for example, “Türkçe” and “Kanada”), not by codes (`tr`, `CA`).

## Rules and skills

- `.claude/rules/` contains automatically loaded, area-specific rules: `testing`, `evidence`, `delivery`, `llm-boundary`, and `frontend`.
- `.claude/skills/` contains procedures for multi-step work: `fix-with-red-test`, `add-audience-field`, and `llm-incident`.
