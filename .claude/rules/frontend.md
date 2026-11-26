---
paths:
  - "frontend/**"
---

# Frontend rules

## Data and live updates

- Do not merge the live preview into persisted state. When a turn finishes, reload its saved result from the API.
- Every message has a `client_turn_id`. Retry an unanswered request or a 5xx response with the same ID; keep the pending turn in session storage.
- Show a turn's events only in its own chat.
- Do not show metrics unsupported by the data or change the workflow. State what each count covers (for example, “unique reachable recipients at draft time”); exclude people who cannot receive the message.

## Copy and language

- Turkish text is the source key: use `t()` or `tk()`, and add the English translation to `src/i18n/en.ts`. `i18n.spec.ts` catches missing translations.
- Show language and country names to users (for example, Türkçe and Kanada), not codes (`tr`, `CA`).
- Label a canned draft in mock mode “Mock · hazır demo taslağı”; do not present it as if the model interpreted the request.

## Visual design

- Light theme only. Use a navy sidebar (`#182242`), page background (`#F6F7FB`), white cards with thin borders, subtle shadows and generous spacing, dark navy text, and a soft purple accent (`#6C5CE7`).
- Use blue for SMS, orange for email, and green for success. Base font size: 14px. Use ECharts for charts.
- Do not make technical details such as workers and rate limits the focus of primary screens.

## Accessibility

- Manage focus in modals and drawers, make charts usable by keyboard, and support `prefers-reduced-motion`.
