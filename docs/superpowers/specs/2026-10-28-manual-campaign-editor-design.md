# Manual Campaign Editor

## Purpose

The marketer can start a campaign entirely by hand or revise any field of an AI-generated draft. Manual work does not call an LLM. A saved draft can later be revised through the assistant. Sending always requires explicit approval.

## Scope

- Create a draft globally in a new chat or inside the current assistant chat.
- Edit the draft name, audience rule, SMS, email subject and body, and active channels.
- Build audience rules through a guided form using the backend's allowed fields, operators, events, and canonical locations. No SQL or raw JSON is shown to the marketer.
- Preserve the existing campaign selector and multi-campaign chat behavior.
- Save each edit as an immutable version with `source=user`; keep previous versions visible.
- Show the audience count and per-channel reachable counts after every successful save.
- Approve only the current valid version; approved content remains read-only.
- Offer an assistant entry from a manual draft so AI can suggest a subsequent version.

## Backend boundary

Go validates and normalizes rules, compiles only whitelisted conditions to parameterized SQL, recomputes the audience, and validates content before saving. The same approval service rechecks rules, rendered limits, consent, and duplicate suppression. A failed validation or stale version creates no new version. Manual creation creates no synthetic assistant or user message. Chats containing only a manual campaign remain discoverable in the chat list.

## Frontend boundary

A shared editor serves assistant and campaign detail. The rule builder reads catalog metadata from the backend and round-trips existing rules. Creation uses an optional chat ID: omitted creates a new chat, specified adds to that chat. The editor distinguishes save from approve and reports validation errors at the relevant field.

## Verification

Backend integration tests cover manual create and edit, audience recount, invalid and stale saves, chat discovery, and approved immutability. Frontend tests cover rule round-tripping, both creation entries, shared editing and approval separation. Fresh `make test` and Go formatting/vet are required. No git staging or commits are authorized.
