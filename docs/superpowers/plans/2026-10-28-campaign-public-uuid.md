# Campaign Public UUID Implementation Plan

> **For agentic workers:** Use TDD for every behavior change. Do not commit,
> stage, push, reset, or delete user data; the owner handles version control.

**Goal:** Use UUID strings as every client-facing campaign identifier while
keeping internal numeric relationships. Existing demo data may be discarded;
the migration also supports it without extra complexity.

**Architecture:** Add a unique `public_id` to `campaigns`. Keep internal
`id`/foreign keys numeric. Resolve incoming UUIDs and map outgoing IDs in the
HTTP adapter; keep the frontend contract entirely UUID-based.

**Tech Stack:** PostgreSQL 17, Go, Vue 3, Vitest, Docker Compose.

---

### Task 1: Database

- [x] Write a failing migration test with an existing campaign, dependent
  version/delivery or turn, and two distinct campaigns.
- [x] Add `00003_campaign_public_id.sql` to backfill and constrain UUIDs.
- [x] Run the migration test and check old foreign keys still point to the
  same internal rows.

### Task 2: Backend API

- [x] Write failing HTTP tests for UUID in chat, campaign detail/list,
  requests, dashboard, turn result, and SSE events; numeric URL rejected.
- [x] Resolve incoming UUIDs at REST, filter, and turn-request boundaries.
- [x] Map outgoing campaign IDs explicitly to UUIDs, preserving numeric
  version IDs and internal model/store IDs.
- [x] Run HTTP and backend tests.

### Task 3: Frontend

- [x] Change campaign ID types, routes, filters, stores, components and turn
  client to UUID strings. Keep ordinal/version numbers as numbers.
- [x] Update tests to assert UUID URL, payload and campaign selection.
- [x] Run typecheck and frontend tests.

### Task 4: Integration

- [x] Run `make test` and a production build.
- [x] Apply migration with `make up` without resetting volumes and check
  campaign rows and API responses.
- [x] Review all client-facing payloads for numeric campaign IDs and update
  README/decision notes. Leave all work uncommitted.
