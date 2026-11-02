import type { CampaignStatus } from '../api/types'
import { intlLocale, t } from '../i18n'
import { campaignStatus } from './format'

// Scheduled sending: the marketer picks a date and time on this device's
// clock; the API takes and keeps the instant in UTC.

// toUTC turns a date-time input value in local time ("2026-11-03T14:30")
// into a UTC instant, or null when it holds none.
export function toUTC(local: string): string | null {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/.test(local)) return null
  const d = new Date(local) // no offset: local time
  return isNaN(d.getTime()) ? null : d.toISOString()
}

const pad = (n: number) => String(n).padStart(2, '0')

// localInputValue writes d as a date-time input value in local time.
export function localInputValue(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

// scheduledLater tells whether a campaign's sending still waits for its time.
export function scheduledLater(scheduledAt: string | null | undefined, now = Date.now()): boolean {
  if (!scheduledAt) return false
  const at = Date.parse(scheduledAt)
  return !isNaN(at) && at > now
}

// dateTimeText writes an instant as a date and time on this device's clock.
export function dateTimeText(iso: string): string {
  const d = new Date(iso)
  return isNaN(d.getTime()) ? iso : d.toLocaleString(intlLocale(), { dateStyle: 'long', timeStyle: 'short' })
}

// zoneName is this device's time zone, the one a date-time input uses.
export function zoneName(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  } catch {
    return 'UTC'
  }
}

// statusLabel names a campaign's status; an approved campaign whose sending
// waits for its time is "scheduled".
export function statusLabel(status: CampaignStatus, scheduledAt?: string | null, now = Date.now()): string {
  return status === 'approved' && scheduledLater(scheduledAt, now) ? t('Zamanlandı') : campaignStatus[status]
}

// MAX_AHEAD_MS mirrors the API's bound on how far ahead a campaign can be
// scheduled (campaign.MaxScheduleAhead).
export const MAX_AHEAD_MS = 365 * 24 * 60 * 60 * 1000
