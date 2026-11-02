import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import StatTile from './StatTile.vue'
import StackedColumns from './StackedColumns.vue'
import CityBars from './CityBars.vue'
import SegmentBar from './SegmentBar.vue'
import EChart from './EChart.vue'
import type { AnalyticsCities, SeriesPoint } from '../../api/types'

describe('StatTile', () => {
  it('shows the count in Turkish digits with its channel split, and says what it counts', async () => {
    const w = mount(StatTile, {
      props: { label: 'Gönderildi', value: 1240, icon: 'sent', tone: 'good', info: 'Provider kabul etti', split: { sms: 1000, email: 240 } },
    })
    expect(w.get('.value').text()).toBe('1.240')
    expect(w.text()).toContain('SMS 1.000')
    expect(w.text()).toContain('E-posta 240')
    expect(w.classes()).toContain('tone-good')
    expect(w.text()).toContain('Provider kabul etti')
    await w.trigger('click')
    expect(w.emitted('select')).toHaveLength(1)
  })
})

const day = (at: string, sms: number, email: number): SeriesPoint => ({ at, sms, email })
const stubbed = { global: { stubs: { EChart: true } } }
type Axis = { data: string[] }

describe('StatTile as a figure', () => {
  it('can be a plain figure rather than a filter button', () => {
    const w = mount(StatTile, { props: { label: 'Gönderildi', value: 5, icon: 'sent', tone: 'good', info: 'x', interactive: false } })
    expect(w.element.tagName).toBe('DIV')
    expect(w.attributes('aria-pressed')).toBeUndefined()
  })
})

describe('StackedColumns', () => {
  const points = [day('2026-10-25', 50, 25), day('2026-10-26', 0, 0), day('2026-10-27', 100, 0)]

  it('hands the chart its columns, with a legend and a summary for screen readers', () => {
    const w = mount(StackedColumns, { props: { points, bucket: 'day' }, ...stubbed })
    const chart = w.getComponent(EChart)
    expect((chart.props('option').xAxis as Axis).data).toEqual(['25 Eki', '26 Eki', '27 Eki'])
    expect(chart.props('label')).toContain('3 gün, toplam 175 gönderim')
    expect(w.findAll('.legend-item').map((l) => l.text())).toEqual(['SMS', 'E-posta'])
  })

  it('reads the same numbers as a table', async () => {
    const w = mount(StackedColumns, { props: { points, bucket: 'day' }, ...stubbed })
    await w.get('button.table-toggle').trigger('click')
    expect(w.findComponent(EChart).exists()).toBe(false)
    const rows = w.findAll('tbody tr').map((r) => r.findAll('td').map((td) => td.text()))
    expect(rows[0]).toEqual(['25 Eki', '50', '25', '75'])
  })

  it('names hours when it counts by hour, and shows only the chosen channel', async () => {
    const w = mount(StackedColumns, { props: { points: [{ at: '2026-10-27T14:00', sms: 3, email: 0 }], bucket: 'hour', channels: ['sms'] }, ...stubbed })
    expect(w.findAll('.legend-item').map((l) => l.text())).toEqual(['SMS'])
    await w.get('button.table-toggle').trigger('click')
    expect(w.get('tbody td').text()).toBe('14:00')
  })
})

const cities: AnalyticsCities = {
  top: [
    { geoname_id: 745044, name: 'İstanbul', country: 'TR', admin1: 'TR-34', sms: 3, email: 1, total: 4 },
    { geoname_id: 323786, name: 'Ankara', country: 'TR', admin1: 'TR-06', sms: 1, email: 1, total: 2 },
    { geoname_id: 999000111, name: '', sms: 0, email: 1, total: 1 },
  ],
  other: { cities: 6, total: 9 },
  unknown: { total: 2, sms: 2, email: 0 },
}

describe('CityBars', () => {
  it('gives the chart a row per city, the rest and the unknown last, and a height that fits them', () => {
    const w = mount(CityBars, { props: { cities }, ...stubbed })
    const chart = w.getComponent(EChart)
    const [names] = chart.props('option').yAxis as Axis[]
    expect(names.data).toEqual(['İstanbul', 'Ankara', 'GeoNames #999000111', 'Diğer 6 şehir', 'Şehri bilinmeyen'])
    const few = mount(CityBars, { props: { cities: { ...cities, top: cities.top.slice(0, 1) } }, ...stubbed })
    expect(chart.props('height')).toBeGreaterThan(few.getComponent(EChart).props('height'))
  })

  it('says what an unknown city means, only when there is one', () => {
    expect(mount(CityBars, { props: { cities }, ...stubbed }).get('.chart-foot').text()).toContain('Şehri bilinmeyen')
    const known = mount(CityBars, { props: { cities: { ...cities, unknown: { total: 0, sms: 0, email: 0 } } }, ...stubbed })
    expect(known.find('.chart-foot').exists()).toBe(false)
  })

  it('reads the same numbers as a table, unknown cities apart', async () => {
    const w = mount(CityBars, { props: { cities }, ...stubbed })
    await w.get('button.table-toggle').trigger('click')
    const rows = w.findAll('tbody tr').map((r) => r.findAll('td').map((td) => td.text()))
    expect(rows.map((r) => r[0])).toEqual(['İstanbul', 'Ankara', 'GeoNames #999000111', 'Diğer 6 şehir', 'Şehri bilinmeyen'])
    expect(rows[4]).toEqual(['Şehri bilinmeyen', '2', '0', '2'])
  })
})

describe('SegmentBar', () => {
  it('draws each part in proportion and names them for screen readers', () => {
    const w = mount(SegmentBar, {
      props: {
        segments: [
          { key: 'sent', label: 'Gönderildi', value: 250, tone: 'good' },
          { key: 'failed', label: 'Başarısız', value: 0, tone: 'critical' },
          { key: 'rest', label: 'Sırada', value: 50, tone: 'track' },
        ],
      },
    })
    const segs = w.findAll('.segment')
    expect(segs).toHaveLength(2)
    expect(segs[0].attributes('style')).toContain('flex-grow: 250')
    expect(w.attributes('aria-label')).toBe('Gönderildi 250, Sırada 50')
  })
})
