<script lang="ts">
// A progress bar in parts (sent, failed, unknown, still queued…), each in
// proportion, with a 2px gap between them. The parts are named for screen
// readers; the visible legend lives with the bar's owner.
export interface Segment {
  key: string
  label: string
  value: number
  tone: 'good' | 'critical' | 'warning' | 'neutral' | 'track' | 'sms' | 'email'
}
</script>

<script setup lang="ts">
import { computed } from 'vue'
import { formatCount } from '../../lib/format'
import { t } from '../../i18n'

const props = defineProps<{ segments: Segment[] }>()

const visible = computed(() => props.segments.filter((s) => s.value > 0))
const label = computed(() => visible.value.map((s) => `${t(s.label)} ${formatCount(s.value)}`).join(', '))
</script>

<template>
  <div class="segment-bar" role="img" :aria-label="label">
    <span
      v-for="s in visible"
      :key="s.key"
      class="segment"
      :class="'tone-' + s.tone"
      :style="{ flexGrow: s.value }"
      :title="`${t(s.label)}: ${formatCount(s.value)}`"
    />
  </div>
</template>
