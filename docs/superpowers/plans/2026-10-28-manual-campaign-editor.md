# Manual Campaign Editor Implementation Plan

> **For agentic workers:** REQUIRED: Use $subagent-driven-development (if subagents available) or $executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Marketers can create campaigns without an LLM, edit every AI-generated draft field including its audience, save immutable versions, and approve explicitly.

**Architecture:** Extend the existing Go draft/version and approval services, preserving backend validation and audience counting. Expose a canonical rule catalog to a structured Vue editor shared by assistant and campaign detail. Manual saves have `source=user` and create no synthetic chat messages. Existing assistant can later edit the manual draft.

**Tech Stack:** Go, pgx/PostgreSQL, Vue 3, TypeScript, Vitest.

**Constraint:** User explicitly forbids git staging, commits, push, and history changes. Existing branch has no commits; no worktree or commits will be made.

---

## Chunk 1: Backend

### Task 1: Full manual draft mutation

**Files:** `backend/internal/campaign/approve.go`, `backend/internal/campaign/approve_test.go`, `backend/internal/segment/catalog.go`

- [ ] Add a failing test: manual create validates a structured rule, counts audience, saves `source=user`, and makes no LLM call.
- [ ] Run `go test ./internal/campaign -run 'Manual' -count=1` from `backend`; confirm expected failure.
- [ ] Implement a shared manual draft service for create/edit. Reuse `segment.Validate`, `segment.Count`, content validation, and `SaveDraft`; require base version on edits.
- [ ] Add failing tests for edited rule changing audience, invalid rule/content leaving no version, stale base version, and approved immutability. Make green.

### Task 2: HTTP and canonical catalog

**Files:** `backend/internal/httpapi/httpapi.go`, `backend/internal/httpapi/campaigns.go`, `backend/internal/httpapi/httpapi_test.go`, `backend/internal/httpapi/analytics.go`, `backend/internal/geo/geo.go`

- [ ] Add failing HTTP tests for POST manual create (new chat and existing chat), full rule edit, catalog metadata, and chat discovery without messages.
- [ ] Run focused `go test ./internal/httpapi -run 'Manual|Catalog|ListChats' -count=1`; confirm feature failures.
- [ ] Add POST `/api/campaigns`, full rule support to POST versions, and GET `/api/campaign-rules/catalog`. Return public UUIDs. Include chats containing campaigns in `/api/chats`.
- [ ] Run focused tests and `go test ./...`; make green. Preserve approve behavior.

## Chunk 2: Frontend

### Task 3: Structured rule editor and shared draft form

**Files:** `frontend/src/components/campaign/RuleBuilder.vue`, `frontend/src/components/campaign/CampaignEditor.vue`, their specs, `frontend/src/api/http.ts`, `frontend/src/api/types.ts`

- [ ] Write failing component tests for all/any, attribute/location/event conditions, selected canonical places, full draft save and validation errors.
- [ ] Run `npm test -- --run` from `frontend`; confirm expected failures.
- [ ] Implement catalog-driven rule builder and shared name/SMS/email/rule form. Use existing API error display patterns.
- [ ] Re-run focused tests and typecheck.

### Task 4: Entry points and detail

**Files:** `frontend/src/components/CampaignPanel.vue`, `frontend/src/views/CampaignDetailView.vue`, `frontend/src/views/WorkspaceView.vue`, `frontend/src/components/shell/AppShell.vue`, `frontend/src/router.ts`, related specs.

- [ ] Write failing tests for manual create from global Campaigns entry and current chat, draft edit in detail/assistant, save creating next version, AI refinement link, and approved read-only view.
- [ ] Run focused Vitest and confirm failures.
- [ ] Wire shared editor and entry points. Keep campaign selection behavior. Make approval separate from save.
- [ ] Re-run tests and typecheck.

## Chunk 3: Verification

- [ ] Run `make test` fresh and inspect failures.
- [ ] Run `go vet ./...` and `gofmt -l` in backend.
- [ ] Verify manual create → edit → approve → worker/dashboard through existing integration tests or a browser scenario.
- [ ] Review against spec and report any limitations. Do not stage, commit, or push.
