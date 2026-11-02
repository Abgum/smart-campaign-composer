import type { AnalyticsCities, CityCount, SeriesPoint } from '../api/types'
import type { ChartOption } from './echarts'
import { bucketLabel, bucketTitle, formatCount, percent } from './format'
import { t } from '../i18n'

// ECharts options for the overview's two charts, built from API data alone.
// The marks follow the dataviz specs: SMS then e-mail in a fixed order and
// color, thin bars with a 2px surface gap between stacked parts, a rounded
// end only on the outer part, recessive axes and grid.

export type Channel = 'sms' | 'email'

// The stylesheet's tokens as plain values (ECharts paints SVG attributes,
// which cannot read CSS variables). Keep in step with styles.css.
export const chartColors = {
  sms: '#2a78d6',
  email: '#eb6834',
  neutral: '#8b92a8',
  ink: '#182242',
  ink2: '#4b5470',
  ink3: '#636b81',
  hairline: '#e7e9f2',
  axis: '#d5d9e6',
  surface: '#ffffff',
  hover: 'rgba(108, 92, 231, 0.07)',
}

const font = "system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif"
const channelLabel = (c: Channel) => (c === 'sms' ? 'SMS' : t('E-posta'))
const order: Channel[] = ['sms', 'email']
const shownOf = (channels: Channel[]) => order.filter((c) => channels.includes(c))

const esc = (s: string) => s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`)
const tipRows = (rows: [Channel, number][]) =>
  rows.map(([c, n]) => `<span class="tip-row"><i class="key ${c}"></i>${channelLabel(c)} ${formatCount(n)}</span>`).join('')

const tooltip = {
  confine: true,
  backgroundColor: chartColors.surface,
  borderColor: chartColors.hairline,
  borderWidth: 1,
  padding: [10, 12],
  className: 'chart-tooltip',
  textStyle: { color: chartColors.ink2, fontSize: 13, fontFamily: font },
  extraCssText: 'border-radius: 12px; box-shadow: 0 2px 4px rgba(24,34,66,.06), 0 12px 32px rgba(24,34,66,.1);',
}
const axisText = { color: chartColors.ink3, fontSize: 12, fontFamily: font }

// The hovered column or row: an axis tooltip gets every series at that index.
function indexOf(params: unknown): number {
  const first = (Array.isArray(params) ? params[0] : params) as { dataIndex?: number } | undefined
  return first?.dataIndex ?? -1
}

// The part drawn last (on top, or at the end) carries the rounded corners.
function outer(values: Record<Channel, number>, shown: Channel[]): Channel | undefined {
  return [...shown].reverse().find((c) => values[c] > 0)
}

// An empty part draws nothing, not even the surface border that would cut
// the axis line; the outer part of a bar carries the rounded end.
function partStyle(value: number, rounded: boolean, radius: number[]) {
  if (value === 0) return { borderWidth: 0 }
  return rounded ? { borderRadius: radius } : undefined
}

export function dailyTip(points: SeriesPoint[], bucket: 'day' | 'hour', channels: Channel[], index: number): string {
  const p = points[index]
  if (!p) return ''
  const shown = shownOf(channels)
  const total = shown.reduce((n, c) => n + p[c], 0)
  return (
    `<span class="tip-title">${esc(bucketTitle(p.at, bucket))}</span>` +
    `<strong class="tip-value">${t('Toplam {n}', { n: formatCount(total) })}</strong>` +
    tipRows(shown.map((c) => [c, p[c]]))
  )
}

// Sent messages per day (or per hour today): SMS at the bottom, e-mail on
// top, so a column's height is the bucket's total.
export function dailyOption(points: SeriesPoint[], bucket: 'day' | 'hour', channels: Channel[]): ChartOption {
  const shown = shownOf(channels)
  return {
    animationDuration: 300,
    textStyle: { fontFamily: font },
    grid: { left: 4, right: 12, top: 12, bottom: 4, containLabel: true },
    tooltip: {
      ...tooltip,
      trigger: 'axis',
      axisPointer: { type: 'shadow', shadowStyle: { color: chartColors.hover } },
      formatter: (params) => dailyTip(points, bucket, channels, indexOf(params)),
    },
    xAxis: {
      type: 'category',
      data: points.map((p) => bucketLabel(p.at, bucket)),
      axisTick: { show: false },
      axisLine: { lineStyle: { color: chartColors.axis } },
      axisLabel: { ...axisText, margin: 10, hideOverlap: true },
    },
    yAxis: {
      type: 'value',
      min: 0,
      minInterval: 1,
      splitNumber: 4,
      axisLabel: { ...axisText, formatter: (v: number) => formatCount(v) },
      splitLine: { lineStyle: { color: chartColors.hairline } },
    },
    series: shown.map((c) => ({
      name: channelLabel(c),
      type: 'bar' as const,
      stack: 'sent',
      barMaxWidth: bucket === 'hour' ? 16 : 24,
      barCategoryGap: '40%',
      itemStyle: { color: chartColors[c], borderColor: chartColors.surface, borderWidth: 1 },
      emphasis: { disabled: true },
      data: points.map((p) => ({ value: p[c], itemStyle: partStyle(p[c], outer(p, shown) === c, [4, 4, 0, 0]) })),
    })),
  }
}

export interface CityRow {
  kind: 'city' | 'other' | 'unknown'
  label: string
  sms: number
  email: number
  total: number
}

export const unknownCityText = () => t('Alıcının şehir kaydı yok (yalnız il bilgisi var); şehir, onay anındaki kayıttan alınır.')
const cityName = (c: CityCount) => c.name || `GeoNames #${c.geoname_id}`

export function cityTip(row: CityRow, all: number, channels: Channel[]): string {
  const head = `<span class="tip-title">${esc(row.label)}</span><strong class="tip-value">${t('{n} gönderim', { n: formatCount(row.total) })} · ${percent(row.total, all)}</strong>`
  if (row.kind === 'other') return head + `<span class="tip-note">${t('İlk sıradakilerin dışında kalan şehirler')}</span>`
  const rows = tipRows(shownOf(channels).map((c) => [c, row[c]]))
  return row.kind === 'unknown' ? head + rows + `<span class="tip-note">${esc(unknownCityText())}</span>` : head + rows
}

// Where sent messages went, by the recipient's city, most first. Each bar is
// split by channel; the cities below the ranking fold into one row that is
// only a number; recipients without a known city get a neutral bar of their
// own on the same scale, never ranked. Totals sit in a column at the end.
export function cityOption(cities: AnalyticsCities, channels: Channel[]): { option: ChartOption; rows: CityRow[] } {
  const shown = shownOf(channels)
  const sum = (r: { sms: number; email: number }) => shown.reduce((n, c) => n + r[c], 0)
  const rows: CityRow[] = cities.top.map((c) => ({ kind: 'city', label: cityName(c), sms: c.sms, email: c.email, total: sum(c) }))
  if (cities.other.cities > 0 && cities.other.total > 0) {
    rows.push({ kind: 'other', label: t('Diğer {n} şehir', { n: cities.other.cities }), sms: 0, email: 0, total: cities.other.total })
  }
  const unknown = sum(cities.unknown)
  if (unknown > 0) rows.push({ kind: 'unknown', label: t('Şehri bilinmeyen'), sms: cities.unknown.sms, email: cities.unknown.email, total: unknown })
  const all = rows.reduce((n, r) => n + r.total, 0)
  const label = { ...axisText, color: chartColors.ink, fontSize: 13 }

  const option: ChartOption = {
    animationDuration: 300,
    textStyle: { fontFamily: font },
    grid: { left: 0, right: 4, top: 0, bottom: 0, containLabel: true },
    tooltip: {
      ...tooltip,
      trigger: 'axis',
      axisPointer: { type: 'shadow', shadowStyle: { color: chartColors.hover } },
      formatter: (params) => {
        const row = rows[indexOf(params)]
        return row ? cityTip(row, all, channels) : ''
      },
    },
    xAxis: { type: 'value', show: false, min: 0 },
    yAxis: [
      {
        type: 'category',
        inverse: true,
        data: rows.map((r) => r.label),
        axisTick: { show: false },
        axisLine: { show: false },
        axisLabel: {
          ...label,
          margin: 14,
          width: 120,
          overflow: 'break',
          color: (_: unknown, i?: number) => (rows[i ?? 0]?.kind === 'city' ? chartColors.ink : chartColors.ink2),
        },
      },
      {
        type: 'category',
        inverse: true,
        position: 'right',
        data: rows.map((r) => formatCount(r.total)),
        axisTick: { show: false },
        axisLine: { show: false },
        axisLabel: { ...label, fontWeight: 600, margin: 10 },
      },
    ],
    series: [
      ...shown.map((c) => ({
        name: channelLabel(c),
        type: 'bar' as const,
        stack: 'city',
        barMaxWidth: 20,
        barCategoryGap: '45%',
        itemStyle: { color: chartColors[c], borderColor: chartColors.surface, borderWidth: 1 },
        emphasis: { disabled: true },
        data: rows.map((r) => {
          const value = r.kind === 'city' ? r[c] : 0
          return { value, itemStyle: partStyle(value, outer(r, shown) === c, [0, 4, 4, 0]) }
        }),
      })),
      {
        name: t('Şehri bilinmeyen'),
        type: 'bar' as const,
        stack: 'city',
        barMaxWidth: 20,
        itemStyle: { color: chartColors.neutral, borderRadius: [0, 4, 4, 0] },
        emphasis: { disabled: true },
        data: rows.map((r) => ({ value: r.kind === 'unknown' ? r.total : 0 })),
      },
    ],
  }
  return { option, rows }
}

// The same readings as plain text, for a screen reader stepping through a
// chart by keyboard.
const plain = (rows: [Channel, number][]) => rows.map(([c, n]) => `${channelLabel(c)} ${formatCount(n)}`).join(', ')

export function dailyText(points: SeriesPoint[], bucket: 'day' | 'hour', channels: Channel[], index: number): string {
  const p = points[index]
  if (!p) return ''
  const shown = shownOf(channels)
  const total = shown.reduce((n, c) => n + p[c], 0)
  return `${bucketTitle(p.at, bucket)}: ${t('toplam {n}', { n: formatCount(total) })}; ${plain(shown.map((c) => [c, p[c]]))}`
}

export function cityText(row: CityRow, all: number, channels: Channel[]): string {
  const head = `${row.label}: ${t('{n} gönderim', { n: formatCount(row.total) })} (${percent(row.total, all)})`
  if (row.kind === 'other') return `${head}; ${t('ilk sıradakilerin dışında kalan şehirler')}`
  const text = `${head}; ${plain(shownOf(channels).map((c) => [c, row[c]]))}`
  return row.kind === 'unknown' ? `${text}. ${unknownCityText()}` : text
}
