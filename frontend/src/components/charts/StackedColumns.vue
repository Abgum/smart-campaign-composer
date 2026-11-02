<script setup lang="ts">
import { t } from '../../i18n'
import { computed, ref } from 'vue'
import type { SeriesPoint } from '../../api/types'
import { bucketLabel, formatCount } from '../../lib/format'
import { dailyOption, dailyText, type Channel } from '../../lib/chartOptions'
import EChart from './EChart.vue'

// Sent deliveries per day (or hour), drawn by ECharts: SMS at the bottom,
// e-mail on top. Hovering a column reads it out; the same numbers are one
// click away as a table.
const props = withDefaults(defineProps<{ points: SeriesPoint[]; bucket: 'day' | 'hour'; channels?: Channel[] }>(), {
  channels: () => ['sms', 'email'],
})

const series = computed(() =>
  ([
    { key: 'sms', label: 'SMS' },
    { key: 'email', label: t('E-posta') },
  ] as const).filter((s) => props.channels.includes(s.key)),
)
const total = (p: SeriesPoint) => series.value.reduce((n, s) => n + p[s.key], 0)
const option = computed(() => dailyOption(props.points, props.bucket, props.channels))
const showTable = ref(false)
const summary = computed(() => {
  const sum = props.points.reduce((n, p) => n + total(p), 0)
  const key = props.bucket === 'hour' ? '{n} saat, toplam {total} gönderim. Ayrıntı için Tablo görünümünü açın.' : '{n} gün, toplam {total} gönderim. Ayrıntı için Tablo görünümünü açın.'
  return t(key, { n: props.points.length, total: formatCount(sum) })
})
</script>

<template>
  <figure class="chart columns-chart">
    <div class="chart-head">
      <ul class="legend">
        <li v-for="s in series" :key="s.key" class="legend-item"><span class="swatch" :class="s.key" aria-hidden="true" />{{ s.label }}</li>
      </ul>
      <button type="button" class="link table-toggle" :aria-pressed="showTable" @click="showTable = !showTable">
        {{ showTable ? t('Grafik') : t('Tablo') }}
      </button>
    </div>

    <EChart
      v-if="!showTable"
      :option="option"
      :height="260"
      :label="summary"
      :items="points.length"
      :describe="(i: number) => dailyText(points, bucket, channels, i)"
    />

    <table v-else class="chart-table">
      <thead>
        <tr>
          <th scope="col">{{ bucket === 'hour' ? t('Saat') : t('Gün') }}</th>
          <th v-for="s in series" :key="s.key" scope="col">{{ s.label }}</th>
          <th scope="col">{{ t('Toplam') }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="p in points" :key="p.at">
          <td>{{ bucketLabel(p.at, bucket) }}</td>
          <td v-for="s in series" :key="s.key">{{ formatCount(p[s.key]) }}</td>
          <td>{{ formatCount(total(p)) }}</td>
        </tr>
      </tbody>
    </table>
  </figure>
</template>
