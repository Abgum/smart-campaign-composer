// The overview's filters live in the URL (?range=7d&channel=sms&campaign=<uuid>),
// so a reload or a shared link shows the same slice. Every widget below the
// filter row reads the same slice. One past day, by hour, is
// ?range=day&day=2026-10-25; the overview opens it from the daily chart.
// Names and values are the API's.

import { tk } from '../i18n'

export type Range = 'today' | 'day' | '7d' | '30d' | '90d' | 'all'
export type Channel = '' | 'sms' | 'email'

export interface Filters {
  range: Range
  day?: string // 2026-10-25, read only when range is 'day'
  channel: Channel
  campaign: string | null
}

export const defaultFilters: Filters = { range: '7d', channel: '', campaign: null }

// ranges are the range buttons; 'day' has none, the daily chart opens it.
// Labels are Turkish source texts; show them with t().
export const ranges: { value: Range; label: string; short: string }[] = [
  { value: 'today', label: tk('Bugün'), short: tk('Bugün') },
  { value: '7d', label: tk('Son 7 gün'), short: tk('7 gün') },
  { value: '30d', label: tk('Son 30 gün'), short: tk('30 gün') },
  { value: '90d', label: tk('Son 90 gün'), short: tk('90 gün') },
  { value: 'all', label: tk('Tümü'), short: tk('Tümü') },
]

export const channels: { value: Channel; label: string }[] = [
  { value: '', label: tk('Tümü') },
  { value: 'sms', label: 'SMS' },
  { value: 'email', label: tk('E-posta') },
]

const one = (v: unknown) => (Array.isArray(v) ? v[0] : v)

// isDay holds for a calendar day written 2026-10-25 (not 2026-02-30).
function isDay(s: string): boolean {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(s)) return false
  const d = new Date(`${s}T00:00:00Z`)
  return !Number.isNaN(d.getTime()) && d.toISOString().slice(0, 10) === s
}

// parseFilters reads the URL query; what it does not know falls back to the
// default.
export function parseFilters(q: Record<string, unknown>): Filters {
  const range = String(one(q.range) ?? '')
  const day = String(one(q.day) ?? '')
  const channel = String(one(q.channel) ?? '')
  const campaign = String(one(q.campaign) ?? '')
  const rest = {
    channel: channel === 'sms' || channel === 'email' ? channel : '',
    campaign: /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(campaign) ? campaign : null,
  } as const
  if (range === 'day' && isDay(day)) return { range, day, ...rest }
  return { range: ranges.some((r) => r.value === range) ? (range as Range) : defaultFilters.range, ...rest }
}

// filtersQuery is the URL query for f; defaults stay out of the URL.
export function filtersQuery(f: Filters): Record<string, string> {
  const q: Record<string, string> = {}
  if (f.range !== defaultFilters.range) q.range = f.range
  if (f.range === 'day' && f.day) q.day = f.day
  if (f.channel) q.channel = f.channel
  if (f.campaign !== null) q.campaign = String(f.campaign)
  return q
}

// analyticsQuery asks GET /api/analytics for the same slice.
export function analyticsQuery(f: Filters, cities: number): string {
  const q = new URLSearchParams({ range: f.range })
  if (f.range === 'day') q.set('day', f.day ?? '')
  q.set('cities', String(cities))
  if (f.channel) q.set('channel', f.channel)
  if (f.campaign !== null) q.set('campaign_id', String(f.campaign))
  return q.toString()
}
