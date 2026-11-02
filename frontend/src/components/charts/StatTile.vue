<script setup lang="ts">
import Icon, { type IconName } from '../Icon.vue'
import { formatCount } from '../../lib/format'
import { t } from '../../i18n'

// A KPI: label, count, channel split. The tone colors only the icon; the
// label says what the number is, so the color never carries it alone.
withDefaults(
  defineProps<{
    label: string
    value: number
    icon: IconName
    tone: 'good' | 'accent' | 'neutral' | 'critical' | 'warning'
    info: string // what exactly is counted
    split?: { sms: number; email: number } | null
    note?: string
    selected?: boolean
    interactive?: boolean // false: a plain figure, not a filter button
  }>(),
  { interactive: true },
)
defineEmits<{ select: [] }>()
</script>

<template>
  <component
    :is="interactive ? 'button' : 'div'"
    :type="interactive ? 'button' : undefined"
    class="stat-tile"
    :class="['tone-' + tone, { selected, static: !interactive }]"
    :aria-pressed="interactive ? !!selected : undefined"
    @click="interactive && $emit('select')"
  >
    <span class="tile-head">
      <span class="tile-icon"><Icon :name="icon" :size="20" /></span>
      <span class="tile-label">{{ label }}</span>
      <span class="tile-info" :title="info"><Icon name="info" :size="14" /><span class="sr-only">{{ info }}</span></span>
    </span>
    <span class="value">{{ formatCount(value) }}</span>
    <span v-if="split" class="split">
      <span><i class="dot-key sms" aria-hidden="true" />SMS {{ formatCount(split.sms) }}</span>
      <span><i class="dot-key email" aria-hidden="true" />{{ t('E-posta') }} {{ formatCount(split.email) }}</span>
    </span>
    <span v-if="note" class="note">{{ note }}</span>
  </component>
</template>
