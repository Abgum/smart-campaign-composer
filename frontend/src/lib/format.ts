import type { CampaignStatus, Condition, PlaceRef } from '../api/types'
import { formatNumber, intlLocale, locale, t, tk } from '../i18n'

// Render only the bold markers used in assistant replies. Vue interpolates
// each segment as text, so model output can never become executable HTML.
export function assistantBoldSegments(text: string): { text: string; bold: boolean }[] {
  const segments: { text: string; bold: boolean }[] = []
  const marker = /\*\*([^*\n]+)\*\*/g
  let position = 0
  for (const match of text.matchAll(marker)) {
    const start = match.index
    if (start > position) segments.push({ text: text.slice(position, start), bold: false })
    segments.push({ text: match[1], bold: true })
    position = start + match[0].length
  }
  if (position < text.length) segments.push({ text: text.slice(position), bold: false })
  return segments
}

export function channelName(c: string): string {
  return c === 'sms' ? 'SMS' : c === 'email' ? t('E-posta') : c
}

// orderedChannels puts SMS first, then e-mail, then anything else.
export function orderedChannels(names: string[]): string[] {
  const rank = (c: string) => (c === 'sms' ? 0 : c === 'email' ? 1 : 2)
  return [...names].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))
}

// campaignStatus names a status in the current language (read on use, so a
// template showing it follows a language change).
export const campaignStatus: Record<CampaignStatus, string> = {
  get draft() {
    return t('Taslak')
  },
  get approved() {
    return t('Onaylandı')
  },
  get sending() {
    return t('Gönderiliyor')
  },
  get completed() {
    return t('Tamamlandı')
  },
  get cancelled() {
    return t('İptal')
  },
}

const fields: Record<string, string> = {
  age: tk('Yaş'),
  total_orders: tk('Toplam sipariş'),
  signup_days_ago: tk('Kayıttan bu yana gün'),
  total_spent: tk('Toplam harcama (TL)'),
  language: tk('Dil'),
}

const operators: Record<string, string> = {
  eq: '=',
  neq: '≠',
  in: '∈',
  not_in: '∉',
  gte: '≥',
  lte: '≤',
  between: tk('arası'),
}

const events: Record<string, string> = {
  cart_add: tk('sepete ekleme'),
  purchase: tk('satın alma'),
  product_view: tk('ürün görüntüleme'),
}

const levels: Record<string, string> = { country: tk('ülke'), admin1: tk('il'), city: tk('şehir') }

const word = (map: Record<string, string>, key: string | undefined) => (key && map[key] ? t(map[key]) : (key ?? ''))

function place(p: PlaceRef): string {
  const name = p.name ?? p.code ?? String(p.geoname_id ?? '')
  return `${name} (${word(levels, p.level)})`
}

// conditionText renders one segment condition for people, e.g.
// "Son 30 günde sepete ekleme, ardından satın alma yok".
export function conditionText(c: Condition): string {
  switch (c.kind) {
    case 'location': {
      const places = (c.places ?? []).map(place).join(', ')
      return c.operator === 'not_in' ? t('Konum dışında: {places}', { places }) : t('Konum: {places}', { places })
    }
    case 'attribute': {
      const f = word(fields, c.field)
      const v = c.values ?? []
      if (c.operator === 'between') return t('{field}: {from}–{to} arası', { field: f, from: v[0], to: v[1] })
      return `${f} ${word(operators, c.operator)} ${v.join(', ')}`
    }
    case 'event': {
      const what = word(events, c.event_type)
      const days = c.within_days ?? 0
      let s = c.negate
        ? t('Son {days} günde {what} yok', { days, what })
        : (c.min_count ?? 1) > 1
          ? t('Son {days} günde {what} (en az {n} kez)', { days, what, n: c.min_count ?? 0 })
          : t('Son {days} günde {what}', { days, what })
      if (c.not_followed_by) s += t(', ardından {what} yok', { what: word(events, c.not_followed_by) })
      return s
    }
  }
  return JSON.stringify(c)
}

// withoutScenario drops the demo model's "#scenario:<name>" tag: it steers the
// mock and is not part of what was asked.
export function withoutScenario(text: string): string {
  return text.replace(/#scenario:\S+/g, '').replace(/\s+/g, ' ').trim()
}

// runeCount counts characters as the SMS limit does (code points).
export function runeCount(s: string): number {
  return [...s].length
}

// templateLength is the length the server's draft check assumes for a
// template: a supported variable counts as 10 characters, whatever its value
// turns out to be at approval, when each rendered message is measured.
const variableEstimate: Record<string, number> = { first_name: 10, last_name: 10, city: 10 }
export function templateLength(s: string): number {
  return runeCount(s.replace(/\{\{\s*([^{}]*?)\s*\}\}/g, (_, name: string) => 'x'.repeat(variableEstimate[name] ?? 0)))
}

export function timeText(iso: string): string {
  const d = new Date(iso)
  return isNaN(d.getTime()) ? iso : d.toLocaleTimeString(intlLocale(), { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

// formatCount writes a count the current language's way: 1.240 or 1,240.
export const formatCount = formatNumber

// percent writes a share the current language's way: %68 or 68%.
export function percent(part: number, total: number): string {
  const v = total > 0 ? Math.round((part / total) * 100) : 0
  return locale.value === 'en' ? `${v}%` : `%${v}`
}

// dates is a date format per language, made once.
function dates(options: Intl.DateTimeFormatOptions) {
  const made = { tr: new Intl.DateTimeFormat('tr-TR', options), en: new Intl.DateTimeFormat('en-US', options) }
  return () => made[locale.value]
}
const shortDay = dates({ day: 'numeric', month: 'short', timeZone: 'UTC' })
const longDay = dates({ day: 'numeric', month: 'long', weekday: 'long', timeZone: 'UTC' })

function utcDay(at: string): Date {
  const [y, m, d] = at.slice(0, 10).split('-').map(Number)
  return new Date(Date.UTC(y, m - 1, d))
}

// bucketLabel names a series point (2026-10-27 or 2026-10-27T14:00) on an
// axis: "27 Eki", or "14:00" by hour. The API already gives local times.
export function bucketLabel(at: string, bucket: 'day' | 'hour'): string {
  return bucket === 'hour' ? at.slice(11, 16) : shortDay().format(utcDay(at))
}

const fullDay = dates({ day: 'numeric', month: 'long', timeZone: 'UTC' })

// dayName names a day (2026-10-25) in a sentence: "25 Ekim".
export function dayName(at: string): string {
  return fullDay().format(utcDay(at))
}

// bucketTitle is the fuller name for a tooltip: "25 Ekim Cumartesi".
export function bucketTitle(at: string, bucket: 'day' | 'hour'): string {
  const day = longDay().format(utcDay(at))
  return bucket === 'hour' ? `${day} ${at.slice(11, 16)}` : day
}

// niceScale rounds an axis up to a clean maximum (1, 2 or 5 × 10ⁿ steps).
export function niceScale(max: number, ticks = 4): { max: number; step: number } {
  if (max <= 0) return { max: 1, step: 1 }
  const raw = max / ticks
  const pow = 10 ** Math.floor(Math.log10(raw))
  const step = Math.max(1, [1, 2, 5, 10].map((m) => m * pow).find((s) => s >= raw)!)
  return { max: Math.ceil(max / step) * step, step }
}

// relativeTime says how long ago, roughly: "3 dk önce".
export function relativeTime(iso: string, now = Date.now()): string {
  const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000))
  if (s < 60) return t('az önce')
  if (s < 3600) return t('{n} dk önce', { n: Math.floor(s / 60) })
  if (s < 86400) return t('{n} sa önce', { n: Math.floor(s / 3600) })
  return t('{n} gün önce', { n: Math.floor(s / 86400) })
}
