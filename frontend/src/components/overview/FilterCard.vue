<script setup lang="ts">
import { t } from '../../i18n'
import { channels, ranges, type Filters } from '../../lib/filters'
import { bucketLabel } from '../../lib/format'

// The white filter card above everything it scopes: date range, channel,
// campaign. One day opened from the daily chart shows as a pressed button
// of its own after the ranges.
const props = defineProps<{ filters: Filters; campaigns: { id: string; label: string }[] }>()
const emit = defineEmits<{ update: [filters: Filters] }>()

function set(patch: Partial<Filters>) {
  emit('update', { ...props.filters, ...patch })
}
</script>

<template>
  <div class="card filter-card">
    <div class="filter-group">
      <span id="filter-range" class="filter-label">{{ t('Tarih aralığı') }}</span>
      <div class="segmented" role="group" aria-labelledby="filter-range">
        <button
          v-for="r in ranges"
          :key="r.value"
          type="button"
          :data-range="r.value"
          :aria-pressed="filters.range === r.value"
          @click="set({ range: r.value, day: undefined })"
        >
          {{ t(r.short) }}
        </button>
        <button v-if="filters.range === 'day' && filters.day" type="button" data-range="day" aria-pressed="true">
          {{ bucketLabel(filters.day, 'day') }}
        </button>
      </div>
    </div>
    <div class="filter-group">
      <span id="filter-channel" class="filter-label">{{ t('Kanal') }}</span>
      <div class="segmented" role="group" aria-labelledby="filter-channel">
        <button
          v-for="c in channels"
          :key="c.value"
          type="button"
          :data-channel="c.value || 'all'"
          :aria-pressed="filters.channel === c.value"
          @click="set({ channel: c.value })"
        >
          <i v-if="c.value" class="dot-key" :class="c.value" aria-hidden="true" />{{ t(c.label) }}
        </button>
      </div>
    </div>
    <div class="filter-group grow">
      <label class="filter-label" for="filter-campaign">{{ t('Kampanya') }}</label>
      <select
        id="filter-campaign"
        name="campaign"
        :value="filters.campaign ?? ''"
        @change="set({ campaign: ($event.target as HTMLSelectElement).value || null })"
      >
        <option value="">{{ t('Tüm kampanyalar') }}</option>
        <option v-for="c in campaigns" :key="c.id" :value="c.id">{{ c.label }}</option>
      </select>
    </div>
  </div>
</template>
