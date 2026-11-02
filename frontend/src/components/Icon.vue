<script lang="ts">
// A small fixed icon set drawn on a 24px grid (stroke icons, currentColor).
// The markup is static; nothing user-provided is ever rendered here.
export type IconName =
  | 'overview'
  | 'campaigns'
  | 'assistant'
  | 'plus'
  | 'sent'
  | 'queued'
  | 'failed'
  | 'unknown'
  | 'warning'
  | 'pin'
  | 'sms'
  | 'mail'
  | 'chevron'
  | 'menu'
  | 'close'
  | 'info'
  | 'external'
  | 'table'
  | 'chart'
  | 'dot'
  | 'check'
  | 'send'
  | 'edit'
  | 'users'

const paths: Record<IconName, string> = {
  overview: '<rect x="3.5" y="3.5" width="7" height="7" rx="1.5"/><rect x="13.5" y="3.5" width="7" height="4.5" rx="1.5"/><rect x="13.5" y="11" width="7" height="9.5" rx="1.5"/><rect x="3.5" y="13.5" width="7" height="7" rx="1.5"/>',
  campaigns: '<path d="M9 6.5h11M9 12h11M9 17.5h11"/><circle cx="4.75" cy="6.5" r="1.25"/><circle cx="4.75" cy="12" r="1.25"/><circle cx="4.75" cy="17.5" r="1.25"/>',
  assistant: '<path d="M4.5 5.5h15a1.5 1.5 0 0 1 1.5 1.5v9a1.5 1.5 0 0 1-1.5 1.5H10l-4.5 3.5v-3.5h-1A1.5 1.5 0 0 1 3 16V7a1.5 1.5 0 0 1 1.5-1.5z"/><path d="M8 10.5h8M8 13.5h5"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  sent: '<circle cx="12" cy="12" r="8.5"/><path d="m8.5 12.25 2.5 2.5 4.75-5"/>',
  queued: '<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>',
  failed: '<circle cx="12" cy="12" r="8.5"/><path d="m9.25 9.25 5.5 5.5M14.75 9.25l-5.5 5.5"/>',
  unknown: '<circle cx="12" cy="12" r="8.5"/><path d="M9.75 9.75a2.35 2.35 0 0 1 4.5.9c0 1.6-2.25 2-2.25 3.35"/><path d="M12 16.75h.01"/>',
  warning: '<path d="M10.6 4.6 3.4 17.4A1.6 1.6 0 0 0 4.8 19.8h14.4a1.6 1.6 0 0 0 1.4-2.4L13.4 4.6a1.6 1.6 0 0 0-2.8 0z"/><path d="M12 9.5v4"/><path d="M12 16.75h.01"/>',
  pin: '<path d="M12 21s-6.5-5.6-6.5-11a6.5 6.5 0 0 1 13 0c0 5.4-6.5 11-6.5 11z"/><circle cx="12" cy="10" r="2.25"/>',
  sms: '<rect x="7" y="2.75" width="10" height="18.5" rx="2"/><path d="M10.75 18h2.5"/>',
  mail: '<rect x="3" y="5.5" width="18" height="13" rx="1.75"/><path d="m3.75 7 8.25 6 8.25-6"/>',
  chevron: '<path d="m7 9.5 5 5 5-5"/>',
  menu: '<path d="M4 7h16M4 12h16M4 17h16"/>',
  close: '<path d="m6.5 6.5 11 11M17.5 6.5l-11 11"/>',
  info: '<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5"/><path d="M12 7.75h.01"/>',
  external: '<path d="M14 4.5h5.5V10"/><path d="M19.5 4.5 11 13"/><path d="M18 13.5V19a1.5 1.5 0 0 1-1.5 1.5H5A1.5 1.5 0 0 1 3.5 19V7.5A1.5 1.5 0 0 1 5 6h5.5"/>',
  table: '<rect x="3.5" y="4.5" width="17" height="15" rx="1.5"/><path d="M3.5 9.5h17M3.5 14.5h17M9.5 9.5v10"/>',
  chart: '<path d="M4 20h16"/><path d="M7 16.5V11M12 16.5V6M17 16.5v-3.5"/>',
  dot: '<circle cx="12" cy="12" r="4" fill="currentColor" stroke="none"/>',
  check: '<path d="m5 12.5 4.5 4.5L19 7.5"/>',
  send: '<path d="M20.5 3.5 10.25 13.75"/><path d="m20.5 3.5-6.25 17-4-6.75-6.75-4z"/>',
  edit: '<path d="M4.5 19.5h4.25L19 9.25a2.3 2.3 0 0 0-3.25-3.25L5.5 16.25z"/><path d="m14.25 7.5 3.25 3.25"/>',
  users: '<circle cx="9" cy="8.5" r="3.25"/><path d="M3.5 19.5a5.5 5.5 0 0 1 11 0"/><path d="M15.5 5.4a3.25 3.25 0 0 1 0 6.2M17.25 14.6a5.5 5.5 0 0 1 3.25 4.9"/>',
}
</script>

<script setup lang="ts">
withDefaults(defineProps<{ name: IconName; size?: number }>(), { size: 18 })
</script>

<template>
  <svg
    class="icon"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.75"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
    v-html="paths[name]"
  />
</template>
