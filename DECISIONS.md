# Architecture decisions

This document explains the main design choices. [README](README.md) covers setup, user-visible behavior, and scope. Each decision records its rationale, alternatives, and trade-offs. Numeric limits are current defaults; package paths are relative to `backend/internal/`.

## Model and conversation

### 1. The backend owns orchestration

**Decision.** The Go backend implements the routing call (`chat/router`), context assembled from PostgreSQL for each turn (`chat/contextbuilder`), model loop (`chat/agentloop`), and durable turn record (`chat/turns`). It does not use LangGraph or the SDK tool runner. The model proposes actions; PostgreSQL holds authoritative state.

**Why.**

- Versions, approval, and deliveries remain in one database and one backend. No second service or state store must be synchronized.
- The flow is short: route, propose, validate, persist. It does not need graph branching or checkpoint recovery.
- The explicit loop enforces these boundaries:
  - Strictly parse accumulated tool input; do not trust schema compliance alone.
  - Check `stop_reason` before executing tools; a refusal or truncated response writes nothing.
  - Retry a `max_tokens` response once with twice the limit, capped at 32,000 tokens.
  - Persist each accepted draft in its own transaction.
  - Limit model iterations to four and end a turn after its third round in which every proposal was rejected; a round that saves a draft does not count.
- The demo model goes through the same validation and persistence path.

**Alternatives.** LangGraph would require a Python or TypeScript service, split state ownership, and move LLM orchestration outside the Go backend. The SDK streaming tool runner would add another iteration policy and a more permissive input path; tying each tool call to the required transaction would need additional hooks.

**Trade-offs and limits.**

- The application maintains its own loop, retry policy, and error classification.
- Context is a selected projection, not a full transcript: the last six turns as text (about 6,000 tokens) and the current versions of at most three target campaigns. Campaign history and the list of campaigns are included in `<chat_state>` in the latest user message, allowing the system prompt to stay stable and cacheable (`cache_control`).
- Earlier turns' thinking and tool blocks are not replayed. Within a turn, thinking blocks are returned unchanged.
- Raw requests and responses are stored in `llm_calls` for inspection, but are not fed back into prompts.

### 2. The router proposes a target; the backend limits writes

**Decision.** A separate model call uses the strict `route_request` tool to propose intent (create, edit, clarify, or chat) and target campaign IDs. The backend checks facts it can prove: whether IDs belong to the chat, whether editable targets are drafts, and whether at most three campaigns are targeted. Invalid routes become clarification questions. Validated targets define the model loop's write scope; it cannot edit another campaign or create an unrequested one.

**Why.** Marketers may identify a campaign by name, order, or distinctive feature. Regex-based language matching is brittle and could silently edit the wrong draft. The model interprets language; the backend enforces the write boundary. Even a wrong route cannot modify an approved campaign or one from another chat.

**Alternatives.** A text-based `TargetResolver` (explicit reference, selected tab, then sole draft) misses indirect references. Cross-checking model output with text rules recreates the same brittle rule engine. Defaulting to the last touched campaign can silently change the wrong one.

**Trade-offs and limits.**

- Routing adds one model call per message (`LLM_ROUTER_EFFORT=low`, `max_tokens` 4,000). An empty chat skips it.
- If routing returns no valid decision because of a refusal, truncation, or malformed JSON, the app asks for clarification. An API failure ends the turn with an error.
- A `#N` reference is treated specially: `#12` is not a visible campaign number, so the backend asks rather than guessing whether it means an internal ID. Recognized forms such as `#12'yi` and `kampanya #12` are Turkish-language patterns.
- With the live Claude model, routing was checked in one chat (create, edit, create, clarify; see [Scope and limitations](README.md#scope-and-limitations)); its accuracy over many messages has not been measured.

### 3. The Claude SDK is behind a narrow LLM interface

**Decision.** Only `llm/anthropic` imports `anthropic-sdk-go/v2`. Both the Claude adapter and `llm/mock` implement `llm.Client`, an interface for one streamed model call, stop reason, and typed errors. Provider choice happens at startup; an in-flight Claude failure never silently switches to mock. Claude calls use adaptive thinking, `LLM_EFFORT` (default `medium`), the SDK's two retries, and a 120-second turn timeout.

**Why.** The official SDK handles streaming and typed API errors. Its version-specific code stays in one package. The loop, validation, and persistence are identical for both providers, so tests need neither a key nor external network access. Message and version records identify which provider generated them.

**Alternatives.** Raw HTTP would require handwritten SSE parsing and error mapping. HTTP recordings would test playback rather than the decision loop. Silent fallback to mock would hide real service failures.

**Trade-offs and limits.** The mock does not interpret natural language. Tests verify the backend around the model, not prompt quality. One live Claude run covered routing, drafting, an edit, approval, and delivery; prompt quality over many briefs has not been measured. See [Scope and limitations](README.md#scope-and-limitations).

### 4. Model output is a proposal, not authority

**Decision.** The main model proposes drafts through `save_campaign_draft` and removes an A/B test through the separate `remove_ab_test` tool (decision 17). The draft tool uses Claude's strict schema mode; the small removal tool does not, because compiling both as strict exceeded the API's grammar limit. Both schemas disallow extra properties, and the backend strictly parses and validates every completed tool input. The draft schema requires critical fields and uses empty values for fields irrelevant to a condition type; this keeps the schema within the API's optional-parameter limit. Strict mode writes properties in schema order, so the adapter sends every object's properties in its `required` order, then its optional ones. A Go map had sent them sorted: the optional `base_version` came before `campaign_id` and could not be added once `campaign_id` was written, and the optional `ab_test` came first. Edits of existing campaigns were refused, or wasted a round, until the model happened to start a call with the right key. `campaign_id` and `base_version` are also required, null for a new campaign. The draft tool runs without `eager_input_streaming`: the API then checks each parameter before streaming it, so the live preview fills in a field at a time (the email's subject and body together) rather than token by token. Eager streaming would send fragments as they are written, but without that check. The backend checks the stop reason, then validates business rules. Validation issues return as `is_error` tool results with `path` and `code`. Unknown tool names are rejected.

**Why.** Strict schemas constrain shape, but cannot establish whether a catalog value is allowed, content meets length limits, or the target is still a current draft. A truncated stream must never be mistaken for a valid proposal. Streaming provides a live preview; the preview is not a saved draft.

**Alternatives.** JSON embedded in free-form response text provides a weaker structure contract. Trusting the schema alone skips backend rules.

**Trade-offs and limits.**

- Every repair needs another model call.
- Personalization variables count as an estimated ten characters in a draft. Approval checks the actual rendered length; a long name can cause a 422 response.
- SMS length counts Unicode code points, not GSM-7/UCS-2 segments. A real carrier may split SMS with Turkish characters sooner.
- Ambiguous place names are returned as candidates; that turn cannot save a draft until the marketer chooses.

### 5. Turns are deduplicated; disconnection does not cancel work

**Decision.**

- Every message carries a client-generated `client_turn_id`, unique with `chat_id`. Retrying the same ID does not run the turn again: an active turn returns 202, a completed turn returns its stored result, and changed content returns `409 idempotency_mismatch`. Only one turn per chat runs at a time.
- A turn runs independently of its HTTP connection, for at most 120 seconds. It writes a heartbeat every five seconds. A turn whose heartbeat is over 30 seconds old is marked `interrupted`. On shutdown the API waits for running turns to store their result; Docker's `stop_grace_period` (150 seconds) leaves it that time.
- Streamed events are provisional. If a slow client fills the 1,024-event buffer, live events stop and a `resync` event tells the client to discard its preview and fetch `GET /api/turns/{id}`. Terminal `error` and `done` events are preserved.
- If a later error occurs after a draft was saved, that version remains and the turn ends `partial`. “Retry” is offered only for a `failed` turn that saved nothing.

**Why.** Reloading a page must not produce a second draft, and a dropped connection must not abandon server work. A stream with missing events needs an explicit recovery path. Rolling back a version already shown to the marketer would make the UI misleading; a partial result reports what actually persisted.

**Alternatives.** A durable event log with `Last-Event-ID` would allow exact stream replay but add storage and complexity. Other options were canceling on disconnect or rolling back the whole turn after an error.

**Trade-offs and limits.** A disconnected user loses the remaining live preview but can fetch the final result. An active turn cannot be canceled from the UI.

## Campaign and approval

### 6. The audience rule is JSON; the backend compiles SQL

**Decision.** Audience rules use a small JSON language: one flat group (`match: all | any`), at most ten conditions, and three kinds of condition: `attribute` (age, order count, spend, days since sign-up, recorded language preference), `event` (`cart_add`, `purchase`, `product_view`; 1–365 days, `min_count`, `negate`, `not_followed_by`), and `location`. The backend strictly parses and validates the rule against a catalog. `segment.Compile` produces parameterized SQL; audience counts use a three-second `statement_timeout`.

**Why.**

- The model cannot submit SQL. The allowlisted compiler is small enough to audit and test, and cannot request arbitrary tables or unbounded query shapes.
- The catalog drives prompts, validation, compilation, and the manual editor via `/api/campaign-rules/catalog`.
- `not_followed_by` correctly means a `cart_add` event in the time window with no later `purchase`. “No purchases in the last 30 days” would incorrectly exclude someone who bought something, then abandoned a new cart.

**Alternatives.** Model-generated SQL leaves a much larger validation surface even with read-only access. Generic rule languages such as JSONLogic or CEL would still require a safe SQL compiler.

**Trade-offs and limits.** Rules cannot nest groups (for example, OR inside AND), and a new field or event needs a code change. The `language` audience filter checks `users.language`, the **recorded preference**, by its primary language, as content selection does: `en-GB` matches `en`. A recipient with no recorded preference does not match `language = en`, even if the campaign would later select English through its country mapping. Audience filtering and per-recipient content selection are separate operations.

### 7. Locations use stable identifiers; new targeting allows countries and cities

**Decision.** The catalog and recipient records store three levels: ISO 3166-1 alpha-2 countries, ISO 3166-2 administrative regions, and GeoNames `geonameId` cities. **New audience rules may target only countries and cities.** Administrative regions remain as metadata and for compatibility; the backend rejects new region targets. A model may propose a city by name or ID, optionally with country context. The backend resolves it to exactly one catalog entry and stores its canonical ID. Ambiguous names require a clarification. A rule has one `location` condition containing its selected places; a city match also checks its country. Each delivery snapshots the recipient's city ID at approval.

**Why.**

- Names alone are ambiguous. The data distinguishes İstanbul province (`TR-34`) from İstanbul city (`745044`), although province targeting is no longer offered. Paris and London also occur in more than one country. Stable IDs avoid spelling and language ambiguity.
- Checking the city and its country together prevents an `any` group from accidentally widening a city target to the whole country.
- The city snapshot keeps historical analytics stable if a recipient moves later.
- An embedded catalog resolves locations consistently without network access.

**Alternatives.** Free-text city names are ambiguous; Turkish plate codes do not cover foreign cities; online geocoding adds cost, a network dependency, and potentially changing results.

**Trade-offs and limits.** The catalog is deliberately small: 81 Turkish provinces as metadata, seeded cities, and a few foreign cities. A new targetable city needs a catalog change. The external GeoNames check verified city ID, name, and country, **not** the city's administrative-region assignment. A recipient with no city ID cannot match a city rule but can match a country rule.

### 8. Every edit creates a version; approval binds to one version

**Decision.** Human and model edits append to `campaign_versions`. Each edit names its base version and fails if that is no longer current. Approval submits the displayed version ID and, in one transaction, revalidates and recomputes the audience (see README, Architecture step 4). Messages are rendered into delivery rows at approval. Approved campaigns cannot receive a new version; approving the same version twice returns stored counts.

**Why.**

- The marketer approves exactly the content sent. The worker does not render it again, so a later name change cannot alter a message.
- Base-version checks prevent lost updates from concurrent edits; the approval row lock prevents a different version from being approved unnoticed.
- Approval has no TTL. Freshness comes from validation and recipient selection at approval time, not an arbitrary deadline.

**Alternatives.** An expiring approval token could invalidate an unchanged draft without refreshing data. Rendering at send time could change approved content. Updating drafts in place would erase history and weaken concurrency checks.

**Trade-offs and limits.**

- Approval counts may differ from draft counts. The dialog shows language and channel differences between preview and result.
- Audience selection has a 15-second timeout; large selections use `COPY` into a temporary table.
- Approved scheduled campaigns can be canceled before the send time if no delivery has started (decision 16). Sending campaigns cannot be canceled.
- Version immutability is an application rule, not a database constraint; application code never updates version content.

### 9. Manual edits use the same validation and version path

**Decision.** Manual creation (`POST /api/campaigns`) and editing (`POST /api/campaigns/{id}/versions`) use the same draft checks and version mechanism as model proposals. They save a new `source=user` version. A stale `base_version_id` receives 409. Manual creation makes no model call. The rule editor uses the catalog, not raw JSON or SQL. Omitted language settings are inherited from the base version; supplied settings are validated and language counts recalculated. Unsaved draft edits block chat submission and approval.

**Why.** Human and model changes should have identical rules and history. Context is rebuilt from the database each turn, so the model sees the human's latest saved version. Blocking chat while there are unsaved edits prevents the next model version from silently replacing what is visible in the editor.

**Alternatives.** Asking the model to merge unsaved edits risks losing them. Locking the campaign while the editor is open risks abandoned locks. A separate manual data model duplicates validation.

**Trade-offs and limits.** Users must save or discard pending edits before returning to chat. Versions can be listed, but there is no side-by-side version diff.

### 10. Multilingual content stays in one campaign

**Decision.**

- A single-language draft sends one content language to everyone. If the marketer does not specify a language, the prompt requests content in the language of the brief. Chat replies may use a different language from the content (`content_language`).
- A “send in each recipient's language” request produces one campaign, audience rule, and version with per-language content (`per_recipient`). Supported languages are `tr`, `en`, and `fr`; variants share channel selection.
- The model proposes variants and country-to-language mappings. The backend resolves each recipient's language: a supported recorded preference takes priority; if it is absent or unsupported, the campaign's country mapping applies. A country without a mapping creates a `country:<CC>` decision group. A resolved language without content creates `variant:<language>`. Recipients unreachable on every selected channel do not wait for a language decision.
- A draft can be saved with pending groups, but cannot be approved. Approval recomputes language groups and returns `409 language_decision_required` without queuing anything if a new unresolved group appears.
- `users.language` may be null. The migration removed its default and `NOT NULL` constraint without rewriting existing preferences.

**Why.**

- One approval and delivery record keep deduplication and tracking together.
- The backend can verify country codes and available variants, but cannot judge whether a proposed country-language mapping is culturally appropriate. Mappings remain visible and manually editable; recipient assignment is deterministic.
- Unmapped countries are not silently assigned English. A decision such as “assign all pending groups to English” records decisions for the groups known at that time, not future groups.
- The model cannot save a decision for a group that has not appeared in a base version (`ask_user_first`).
- Language counts include only recipients reachable through at least one selected channel, counting each recipient once.

**Alternatives.** Separate campaigns per language split approval and tracking. Silent fallback to English risks the wrong language. Translation at delivery time could send content never approved by a human. Treating every unsupported preference as a separate question was tried, then replaced with the visible country mapping fallback.

**Trade-offs and limits.**

- Even if the marketer includes a group decision in the first message, that group has not yet appeared in a saved base version. The app may ask once more before recording the decision.
- Draft counts are snapshots; approval recalculates them against current recipients.
- Country-language mappings are validated structurally, but their real-world suitability is not automatically verified.
- Prompt instructions govern language choice and clarification wording. Scripted mock scenarios do not establish how reliably a live Claude model follows those instructions.

## Delivery

### 11. Consent is checked twice

**Decision.** Approval excludes recipients without channel consent or contact details. Immediately before every provider call, the worker checks them again; if revoked, it does not call the provider and marks the delivery `skipped_no_consent`. This check precedes the channel rate grant.

**Why.** Minutes may pass between approval and delivery because of rate limits, 429 pauses, or retry waits. Revoked consent must stop a pending call. The approval check keeps displayed counts accurate and avoids unnecessary delivery rows. Checking consent before obtaining a rate grant also leaves no variable-duration database query between grant and call (decision 13).

**Alternatives.** Checking only at approval misses revocations. Checking only at delivery makes approval counts include recipients who cannot receive anything.

**Trade-offs and limits.** Every attempt adds a query. Consent revoked after a provider call begins cannot retract that call. A delivery with an earlier uncertain attempt ends `unknown`, not `skipped_no_consent`, because it may already have arrived (decision 14).

### 12. PostgreSQL is the queue

**Decision.** The `deliveries` table is the queue. Each channel worker claims only as many rows as it has free execution slots, with `SKIP LOCKED` and a 30-second lease; when idle, it polls every 250 ms. A reaper checks expired leases every five seconds and treats those attempts as uncertain. The attempt number fences result writes, so an old worker cannot settle a reclaimed delivery. A provider call starts only when at least 15 seconds remain on the lease, against a ten-second provider timeout. Shutdown, a 429 pause, or a short lease before a call returns the delivery without counting an attempt.

**Why.** Approval can write deliveries and campaign state in one transaction. A separate queue would need an outbox for failures between database and queue writes. Deduplication relies on a database unique index on the same delivery rows, and dashboards read those rows.

**Alternatives.** RabbitMQ, Redis, or Kafka would add a second system plus dual-write or outbox handling; the database would still be needed for deduplication.

**Trade-offs and limits.** Polling adds up to 250 ms of delay. PostgreSQL carries both data and queue load. Concurrency is fixed at 32 per channel. Provider calls are at least once; the idempotency key prevents duplicate acceptance (decision 14).

### 13. Rate limits are shared across workers and restarts

**Decision.** PostgreSQL holds each channel's limit (default 100 call starts per second), its last `calls` grants in `channel_grants`, and `grant_call()`. The function locks the channel row, uses database time, and returns a wait if the oldest grant has not left the sliding window. The grant window includes a 50 ms margin. A worker uses a grant only if it can start the call within 50 ms of requesting it; a late grant is discarded and replaced. The same row stores `paused_until` for 429 responses (`Retry-After`, or one second if absent). A worker configured with a different limit cannot start while another worker runs (a heartbeat in the last 30 seconds). With none running, the new limit replaces the stored one and its grants, and a stopping worker removes its heartbeat, so a changed `*_RATE_PER_SECOND` takes effect when the workers restart.

**Why.**

- A provider limit applies across all workers. An earlier in-memory limiter allowed about 200 calls/second with two workers, or a fresh 100-call burst after restart.
- A token bucket with a full burst can allow nearly 200 calls in a rolling second. The sliding window directly expresses the any-one-second limit.
- Database time avoids worker clock skew. The margin and stale-grant rule aim to apply the limit to actual call starts, not only grant times.

**Alternatives.** Per-worker memory ignores shared limits. Redis would add another system. A token bucket does not enforce this rolling-window contract.

**Trade-offs and limits.** Each call needs a database round trip and waits for a channel-row lock. The margin reduces practical throughput to about 95 calls/second. The limit measures call start time, not arrival at the provider. A process pause between the last freshness check and the call can still shift the start.

### 14. Delivery outcomes follow the evidence available

**Decision.** The worker classifies attempts as follows:

- “Definitely not sent” applies to HTTP 408, 429, other 4xx responses, and errors before a connection is established. `httptrace` observes the connection boundary; errors after that boundary, including writes, are uncertain.
- Uncertainty is sticky: after one uncertain attempt, a delivery may end `sent` or `unknown`, never `failed`. `failed` requires every attempt to be definitely unsent.
- Retries reuse the delivery ID as `Idempotency-Key`. Backoff uses full jitter and never waits less than `Retry-After`.

**Why.** After connection establishment, bytes may have reached the provider. A timeout or 503 is not proof of non-delivery. Incorrectly treating it as safe to resend under a new identity could send the same message twice. Full jitter spreads retries after a common outage.

**Alternatives.** Treating timeouts as definite failures risks duplicate sends; fixed backoff can synchronize retries; new keys per attempt defeat provider deduplication.

**Trade-offs and limits.**

- There is no reconciliation call to query the provider and resolve an `unknown` outcome.
- Safety depends on provider support for idempotency keys. The mock provider writes and syncs acceptances to an append-only log, survives restarts, and replays the first acceptance for a reused key. It recognizes an accepted key before applying scripted 429, 400, or 503 refusals. A real provider would need independent verification of these guarantees.

### 15. “No duplicate message” is content-based across campaigns

**Decision.** A message identity consists of recipient, channel, and a hash of rendered content, independent of campaign. The partial unique index `ux_deliveries_message` admits at most one active or accepted delivery for that identity. Approval counts conflicting rows as duplicate content and skips them. Before hashing, text is NFC-normalized and repeated whitespace collapsed; case and other characters remain distinct. Email subject and body are separate hash inputs. `failed` and `skipped_no_consent` release the claim; `unknown` retains it.

**Why.**

- Campaign-level uniqueness would still allow a copied campaign to send the same SMS twice.
- Under uncertainty, the design prefers the risk of a message not arriving over the risk of sending it twice.
- A database constraint also works when two approvals race.

**Alternatives.** Campaign-scoped deduplication misses copies. A 30-day window weakens the unconditional guarantee. Releasing `unknown` risks a duplicate.

**Trade-offs and limits.**

- An `unknown` message may never have arrived, yet the same recipient and channel cannot receive identical content later.
- There is no expiration window or intentional resend override.
- The same text on another channel, or slightly different text on the same channel, has a different identity.
- If campaign B skips a recipient because campaign A has a pending identical delivery, and A later fails definitively, B's skipped row is not queued again.

## Optional features

### 16. Scheduling is an approval option

**Decision.** Approval defaults to immediate sending, but the marketer may select a future date and time. Validation, audience selection, rendering, consent, and deduplication still happen at approval. Only `next_attempt_at` is deferred. The worker already claims due deliveries, so it needs no separate scheduling service. `campaigns.scheduled_at` is stored and returned in UTC (`timestamptz`). The instant must be in the future and at most 365 days away; otherwise the API returns `422 schedule_in_past` or `schedule_too_far`. Future deliveries appear as `scheduled` rather than “Queued.”

**Why.** Approved content should remain fixed, while consent must be current when a call starts. The queue's due time survives restarts. A duplicate approved in the meantime is excluded because the first scheduled campaign already claims that content identity.

**Alternatives.** Rendering when the time arrives could send different content from what was approved. A scheduler that approves later would make approval counts stale and need a second state transition. An external cron service adds operational complexity.

**Trade-offs and limits.** The audience is frozen at approval. Recipients who later qualify are not added, and recipients who later stop qualifying remain selected; only consent and contact details are rechecked. Before the scheduled time, cancellation is allowed only if no delivery has started; it deletes pending deliveries in one transaction and releases their duplicate-content claims. Rescheduling and cancellation after sending starts are unavailable. The UI interprets dates in the browser's time zone, not `APP_TIMEZONE`.

### 17. A/B uses two arms in one campaign, after language selection

**Decision.** A is the draft's primary content. B is stored in `campaign_versions.ab_test`: one same-language, same-channel B for every A variant. First resolve the recipient's language, then assign an arm through `abtest.Arm(campaign, recipient)` using the first bit of a SHA-256 hash of their IDs. Approval records the chosen arm in `deliveries.ab_variant`; the worker never recomputes it.

- **Validation:** Draft generation, manual edits, and approval require one B per A, matching channels, valid lengths and variables, and at least one text difference from A. Approval also measures rendered B lengths.
- **Counts:** Preview and approval include `variants`; tracking exposes `tracking.Campaign.variants` and campaign detail displays “A/B sonuçları” (“A/B results”).
- **Model edits:** The draft tool accepts `ab_test`. The prompt asks the model not to invent A/B tests and to include existing B on edits. If `ab_test` is omitted, the backend preserves B while holding the campaign lock and checks it against the new A. An incompatible B (channel, language, or identical text) rejects the edit with `ab_test_outdated`; nothing is written. Only `remove_ab_test` removes B through chat, creating a new version with other fields unchanged.
- **Manual edits:** Omitting B preserves it; explicit `null` removes it.

**Why.**

- Recipient-based hashing keeps SMS and email in the same arm. Stateless assignment makes preview, approval, retries, and restarts consistent.
- Including campaign ID makes each campaign a fresh split, so the same recipients do not always get B.
- Language is resolved first, leaving country mapping and pending language groups independent of A/B.
- Content-based deduplication naturally applies per arm: someone who already received A is excluded from A, but may receive distinct B.
- A separate removal tool prevents an omitted field from silently deleting a test. Checking preserved B under the same lock prevents carrying an incompatible old B into a new version.

**Alternatives.** Two separate campaigns would require coordinated audience splitting and could overlap. Splitting the approved recipient list by order gives an exact half but changes assignments when the audience changes. Random allocation could differ between preview and approval. Treating omitted `ab_test` as removal risks accidental deletion; silently rewriting B would make the backend an unapproved copywriter.

**Trade-offs and limits.** The ratio is approximate for small audiences. There are only two arms; there is no winner selection or conversion measure. Prometheus metrics are not split by arm. The model interprets whether the user asked to remove A/B; the backend only checks target, version, and presence of a test. An incompatible B consumes a repair iteration; the third rejection ends the turn.

### 18. CI uses the same make targets as local development

**Decision.** `.github/workflows/ci.yml` runs two jobs on every push and pull request: `make lint` (gofmt, go vet, actionlint) and `make test` (Go `-race` against PostgreSQL plus Vitest and vue-tsc). Everything runs in Docker; the runner does not need a separate Go or Node installation. The workflow reads no secret. `make lint` checks that it still runs both targets and has no secret reference.

**Why.** CI and local runs use the same commands and images. Tests use a mock LLM and local SSE fixtures, so no Claude API key is needed. The lint check guards against accidentally adding a key requirement later.

**Alternatives.** `setup-go` and `setup-node` could simplify caching but introduce another toolchain configuration. More linters are possible.

**Trade-offs and limits.** Pulling images adds runtime; there is no dedicated cache. Backend checks include gofmt and go vet, but not staticcheck. ESLint and Prettier are not configured. The committed workflow was checked locally with actionlint and the same make targets.
