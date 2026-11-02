import { describe, expect, it } from 'vitest'
import type { AnalyticsCities, SeriesPoint } from '../api/types'
import { chartColors, cityOption, cityTip, dailyOption, dailyTip } from './chartOptions'

type Bar = {
  name: string
  stack?: string
  barMaxWidth?: number
  itemStyle: { color: string }
  data: { value: number; itemStyle?: { borderRadius?: number[] } }[]
}
const bars = (o: unknown) => (o as { series: Bar[] }).series
const categories = (axis: unknown) => (axis as { data: string[] }).data

const day = (at: string, sms: number, email: number): SeriesPoint => ({ at, sms, email })
const points = [day('2026-10-25', 50, 25), day('2026-10-26', 0, 0), day('2026-10-27', 100, 0)]

describe('dailyOption', () => {
  it('stacks SMS under e-mail in thin columns, one per day, rounding only the top part', () => {
    const o = dailyOption(points, 'day', ['sms', 'email'])
    expect(categories(o.xAxis)).toEqual(['25 Eki', '26 Eki', '27 Eki'])
    const [sms, email] = bars(o)
    expect([sms.name, email.name]).toEqual(['SMS', 'E-posta'])
    expect([sms.itemStyle.color, email.itemStyle.color]).toEqual([chartColors.sms, chartColors.email])
    expect(sms.stack).toBeTruthy()
    expect(sms.stack).toBe(email.stack)
    expect(sms.barMaxWidth).toBeLessThanOrEqual(24)
    expect(sms.data.map((d) => d.value)).toEqual([50, 0, 100])
    expect(email.data.map((d) => d.value)).toEqual([25, 0, 0])
    expect(email.data[0].itemStyle?.borderRadius).toEqual([4, 4, 0, 0])
    expect(sms.data[0].itemStyle?.borderRadius).toBeUndefined()
    expect(sms.data[2].itemStyle?.borderRadius).toEqual([4, 4, 0, 0]) // no e-mail that day: SMS is the top
  })

  it('draws nothing for an empty part, not even its border over the axis', () => {
    const [sms, email] = bars(dailyOption(points, 'day', ['sms', 'email']))
    expect(sms.data[1].itemStyle).toEqual({ borderWidth: 0 })
    expect(email.data[2].itemStyle).toEqual({ borderWidth: 0 })
  })

  it('labels hours, and leaves out a channel that is filtered away', () => {
    const o = dailyOption([{ at: '2026-10-28T17:00', sms: 3, email: 9 }], 'hour', ['sms'])
    expect(categories(o.xAxis)).toEqual(['17:00'])
    expect(bars(o).map((s) => s.name)).toEqual(['SMS'])
  })

  it('reads a column out in its tooltip', () => {
    const tip = dailyTip(points, 'day', ['sms', 'email'], 0)
    expect(tip).toContain('25 Eki')
    expect(tip).toContain('Toplam 75')
    expect(tip).toContain('SMS 50')
    expect(tip).toContain('E-posta 25')
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

describe('cityOption', () => {
  it('ranks cities top-down on one scale, split by channel, with the totals at the end', () => {
    const { option, rows } = cityOption(cities, ['sms', 'email'])
    const [names, totals] = option.yAxis as { data: string[]; inverse: boolean }[]
    expect(names.data).toEqual(['İstanbul', 'Ankara', 'GeoNames #999000111', 'Diğer 6 şehir', 'Şehri bilinmeyen'])
    expect(names.inverse).toBe(true)
    expect(totals.data).toEqual(['4', '2', '1', '9', '2'])
    expect(rows.map((r) => r.kind)).toEqual(['city', 'city', 'city', 'other', 'unknown'])
    const by = Object.fromEntries(bars(option).map((s) => [s.name, s.data.map((d) => d.value)]))
    expect(by['SMS']).toEqual([3, 1, 0, 0, 0])
    expect(by['E-posta']).toEqual([1, 1, 1, 0, 0])
    // recipients without a city: one neutral bar on the same scale; the folded rest: a number, no bar
    expect(by['Şehri bilinmeyen']).toEqual([0, 0, 0, 0, 2])
    expect(bars(option).find((s) => s.name === 'Şehri bilinmeyen')?.itemStyle.color).toBe(chartColors.neutral)
  })

  it('leaves out rows with nothing in them, and counts only the chosen channel', () => {
    const none = cityOption({ ...cities, other: { cities: 0, total: 0 }, unknown: { total: 0, sms: 0, email: 0 } }, ['sms', 'email'])
    expect(none.rows.map((r) => r.label)).toEqual(['İstanbul', 'Ankara', 'GeoNames #999000111'])
    const email = cityOption(cities, ['email'])
    expect(bars(email.option).map((s) => s.name)).not.toContain('SMS')
    expect(email.rows.map((r) => r.total)).toEqual([1, 1, 1, 9]) // nobody without a city got an e-mail
  })

  it('reads a row out, and says what an unknown city means', () => {
    const { rows } = cityOption(cities, ['sms', 'email'])
    const tip = cityTip(rows[0], 18, ['sms', 'email'])
    expect(tip).toContain('İstanbul')
    expect(tip).toContain('4 gönderim')
    expect(tip).toContain('SMS 3')
    expect(tip).toContain('E-posta 1')
    expect(cityTip(rows[4], 18, ['sms', 'email'])).toContain('şehir kaydı yok')
  })
})
