import type { AnalyticsCities, Counts, TrackedCampaign } from '../api/types'

export type ChannelFilter = '' | 'sms' | 'email'

export interface CampaignTotals {
  target: number // deliveries queued at approval
  sent: number
  failed: number
  unknown: number
  skipped: number // consent withdrawn before sending
  queued: number // pending, in flight or to retry
  done: number
  excludedConsent: number
  excludedDuplicate: number
  progress: number // done / target
}

// campaignTotals sums a campaign's counts, for one channel or both.
export function campaignTotals(c: TrackedCampaign, channel: ChannelFilter = ''): CampaignTotals {
  const parts = Object.entries(c.channels)
    .filter(([ch]) => !channel || ch === channel)
    .map(([, counts]) => counts)
  const sum = (f: (x: Counts) => number) => parts.reduce((n, x) => n + f(x), 0)
  const sent = sum((x) => x.sent)
  const failed = sum((x) => x.failed)
  const unknown = sum((x) => x.unknown)
  const skipped = sum((x) => x.skipped_no_consent)
  const queued = sum((x) => x.pending + x.in_flight + x.retry)
  const done = sent + failed + unknown + skipped
  const target = done + queued
  return {
    target,
    sent,
    failed,
    unknown,
    skipped,
    queued,
    done,
    excludedConsent: sum((x) => x.excluded_consent),
    excludedDuplicate: sum((x) => x.excluded_duplicate),
    progress: target > 0 ? done / target : 0,
  }
}

// foldCities keeps the first n ranked cities and folds the rest into the
// other cities (phones show five).
export function foldCities(c: AnalyticsCities, n: number): AnalyticsCities {
  if (c.top.length <= n) return c
  const rest = c.top.slice(n)
  return {
    top: c.top.slice(0, n),
    other: { cities: c.other.cities + rest.length, total: c.other.total + rest.reduce((s, x) => s + x.total, 0) },
    unknown: c.unknown,
  }
}
