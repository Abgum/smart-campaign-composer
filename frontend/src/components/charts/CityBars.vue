<script setup lang="ts">
import { t } from '../../i18n'
import { computed, ref } from 'vue'
import type { AnalyticsCities, CityCount } from '../../api/types'
import { formatCount } from '../../lib/format'
import { cityOption, cityText, unknownCityText, type Channel } from '../../lib/chartOptions'
import Icon from '../Icon.vue'
import EChart from './EChart.vue'

// Where sent messages went, by the recipient's city (GeoNames), most first,
// drawn by ECharts as horizontal bars split by channel. The chart grows a
// row at a time; the same numbers are one click away as a table.
const props = withDefaults(defineProps<{ cities: AnalyticsCities; channels?: Channel[] }>(), {
  channels: () => ['sms', 'email'],
})

const ROW = 38
const series = computed(() =>
  ([
    { key: 'sms', label: 'SMS' },
    { key: 'email', label: t('E-posta') },
  ] as const).filter((s) => props.channels.includes(s.key)),
)
const chart = computed(() => cityOption(props.cities, props.channels))
const height = computed(() => chart.value.rows.length * ROW + 8)
const all = computed(() => chart.value.rows.reduce((n, r) => n + r.total, 0))
const summary = computed(() => {
  const top = chart.value.rows.filter((r) => r.kind === 'city')
  const first = top[0] ? t('; en çok {name} ({n})', { name: top[0].label, n: formatCount(top[0].total) }) : ''
  return t('{n} şehir{first}. Ayrıntı için Tablo görünümünü açın.', { n: top.length, first })
})
const name = (c: CityCount) => c.name || `GeoNames #${c.geoname_id}`
const showTable = ref(false)
</script>

<template>
  <figure class="chart city-bars">
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
      :option="chart.option"
      :height="height"
      :label="summary"
      :items="chart.rows.length"
      :describe="(i: number) => cityText(chart.rows[i], all, channels)"
    />

    <table v-else class="chart-table">
      <thead>
        <tr>
          <th scope="col">{{ t('Şehir') }}</th>
          <th v-for="s in series" :key="s.key" scope="col">{{ s.label }}</th>
          <th scope="col">{{ t('Toplam') }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="c in cities.top" :key="c.geoname_id">
          <td>{{ name(c) }}</td>
          <td v-for="s in series" :key="s.key">{{ formatCount(c[s.key]) }}</td>
          <td>{{ formatCount(c.total) }}</td>
        </tr>
        <tr v-if="cities.other.cities">
          <td>{{ t('Diğer {n} şehir', { n: cities.other.cities }) }}</td>
          <td v-for="s in series" :key="s.key">–</td>
          <td>{{ formatCount(cities.other.total) }}</td>
        </tr>
        <tr v-if="cities.unknown.total">
          <td>{{ t('Şehri bilinmeyen') }}</td>
          <td v-for="s in series" :key="s.key">{{ formatCount(cities.unknown[s.key]) }}</td>
          <td>{{ formatCount(cities.unknown.total) }}</td>
        </tr>
      </tbody>
    </table>

    <p v-if="cities.unknown.total" class="chart-foot">
      <Icon name="info" :size="14" /><span><strong>{{ t('Şehri bilinmeyen:') }}</strong> {{ unknownCityText() }}</span>
    </p>
  </figure>
</template>
