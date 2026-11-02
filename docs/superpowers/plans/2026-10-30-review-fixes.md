# Campaign Review Fixes Implementation Plan

> **For agentic workers:** REQUIRED: Use $subagent-driven-development (if subagents available) or $executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent loss and duplicate creation of manually edited campaigns, keep streamed previews accurate after truncated model calls, and reject ambiguous `#N` campaign references.

**Architecture:** Guard dirty form transitions in Vue and separate successful mutation responses from refresh failures. Give LLM stream attempts explicit discard events. Validate `#N` ambiguity before accepting a router decision. Existing backend version and approval rules remain authoritative.

**Tech Stack:** Vue 3, Pinia, Vitest; Go, PostgreSQL, Docker test compose.

**Constraint:** No git staging, commits, push, reset, or history changes without user request.

---

## Chunk 1: Forms and mutation result

### Task 1: Preserve unsaved edits

**Files:** `frontend/src/components/CampaignPanel.vue`, `frontend/src/views/CampaignDetailView.vue`, `frontend/src/components/assistant/assistant.spec.ts`, `frontend/src/views/campaigns.spec.ts`

- [x] Write failing tests for switching campaign tabs while editing, sending a chat message while creating a manual draft, and stale 409 on detail edit preserving entered fields.
- [x] Run focused Vitest to verify the expected failures.
- [x] Add explicit save/discard guard on transitions; include creation in `unsavedEdit`; keep stale form values while showing conflict and offer reload.
- [x] Re-run focused tests.

### Task 2: Distinguish save from refresh

**Files:** `frontend/src/stores/chat.ts`, `frontend/src/stores/manual.spec.ts`, `frontend/src/components/CampaignPanel.vue`

- [x] Write failing test: POST create returns 201, subsequent GET fails; no false `Kaydedilemedi` and no duplicate POST on retry.
- [x] Run focused Vitest to verify failure.
- [x] Treat mutation as committed after successful POST. Report refresh failure separately and retain created campaign ID for recovery.
- [x] Re-run focused tests.

## Chunk 2: Model routing and stream

### Task 3: Discard truncated preview

**Files:** `backend/internal/chat/agentloop/agentloop.go`, `backend/internal/chat/agentloop/agentloop_test.go`, `frontend/src/stores/chat.ts`, `frontend/src/stores/chat_preview.spec.ts`

- [x] Write failing Go test: first response streams text/tool input then ends `max_tokens`, second succeeds; a discard event precedes second attempt. Write failing frontend test that removes only that iteration's preview.
- [x] Run focused Go and Vitest; confirm failures.
- [x] Emit and handle attempt discard without deleting valid earlier iterations.
- [x] Re-run focused tests.

### Task 4: Reject ambiguous hash number

**Files:** `backend/internal/chat/router/router.go`, `backend/internal/chat/router/router_test.go`

- [x] Write failing test with `#12`, internal ID 12/ordinal 1 and internal ID 99/ordinal 12, router decision edit ID 12. Expect clarify; no draft write.
- [x] Run focused Go test to verify failure.
- [x] Add a narrow explicit-reference guard before accepting the router decision.
- [x] Re-run focused test.

## Chunk 3: Verification

- [x] Run `make test` fresh, with backend and frontend test projects not competing.
- [x] Run `go vet ./...` and `gofmt -l` in the backend container or available local toolchain.
- [x] Review the four original reproduction scenarios and report any remaining limitation.
