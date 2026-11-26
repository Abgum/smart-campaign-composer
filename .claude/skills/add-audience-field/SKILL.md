---
name: add-audience-field
description: Use when adding a user attribute or event type to the audience rules in smart-campaign-composer.
---

# Add an audience field or event

The catalog (`segment.DefaultCatalog`) is the source of truth. The tool schema, system prompt, and manual editor's field list are generated from it **automatically**; do not edit those lists by hand. The manual steps follow.

Use the `fix-with-red-test` workflow for every step: start with a failing test.

## A. Add a user attribute (for example, `loyalty_tier`)

1. **Write failing tests.**
   - In `segment/compile_test.go`, test that the new attribute selects the right audience and that its value is a bind parameter, absent from the SQL text (follow `TestCompile_ParamsOnly`).
   - In `segment/validate_test.go`, test rejection of disallowed operators and out-of-range values.
2. **Migration.** Add the next numbered migration in `backend/internal/store/migrations/` to add the column to `users`. Do not edit an applied migration.
3. **Catalog.** Add the name, type, operators, minimum/maximum, and English description to `Fields` in `segment/catalog.go`. The description reaches the model and editor.
4. **SQL compiler.** Update `column()` in `segment/compile.go` with the column expression and bind-parameter type (`::int`, `::numeric`, or `::text`). Without this, compilation reports “no column for field.” Never interpolate a value into SQL text.
5. **New type, if needed.** Add type validation in `segment/validate.go` and value conversion in `segment/compile.go`.
6. **Seed.** Add realistic values for demo users in `seed/seed.go` so audience tests remain meaningful.
7. **UI labels.** The editor gets the field from the catalog automatically, but its labels are handwritten. Update:
   - `labels` in `frontend/src/components/campaign/RuleBuilder.vue`,
   - labels in `frontend/src/lib/format.ts`,
   - English translations in `frontend/src/i18n/en.ts`.
   Without a label, the UI shows the raw field name.
8. **Check generated surfaces; do not edit them directly.**
   - Does the field appear in the tool-schema enum (`chat/tools/tools.go`)?
   - Does it appear with its description and operators in the system prompt (`chat/contextbuilder/prompt.go`)?
   - Does `GET /api/campaign-rules/catalog` return it?
   - Does `TestStrictTools_OptionalParametersWithinClaudeLimit` still pass?
9. **Documentation.** Update the attribute list in the segment decision in `DECISIONS.md`.
10. **End-to-end check.** Run `make test`. If possible, build a rule with the new attribute in the UI and check its audience count.

## B. Add an event (for example, `wishlist_add`)

1. **Failing test.** In `segment/compile_test.go`, test an audience with events both inside and outside the time window. If the event supports `not_followed_by`, add an ordered-event scenario.
2. **Migration.** A CHECK constraint enumerates permitted values of `user_events.type`. Add a new migration that extends it.
3. **Catalog.** Add the event name and English description to `Events` in `segment/catalog.go`. Add it to `Sequences` if it can participate in an ordered rule.
4. **Seed.** Give demo users sample events of this type.
5. **UI labels.** Update the same three places as A.7.
6. **Generated surfaces.** Check the same items as A.8.
7. **Documentation.** Update the event list in `DECISIONS.md`.

## Do not

- Do not insert a field name supplied by the model or user directly into SQL. Only fixed expressions from `column()` belong in SQL text.
- Do not add the field directly to the prompt or tool schema; both are generated from the catalog.
- Do not edit an existing migration.
