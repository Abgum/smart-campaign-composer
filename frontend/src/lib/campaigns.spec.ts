// @vitest-environment node
import { describe, expect, it } from 'vitest'
import { campaignTotals, foldCities } from './campaigns'
import type { AnalyticsCities, Counts, TrackedCampaign } from '../api/types'

const counts = (c: Partial<Counts>): Counts => ({
  excluded_consent: 0,
  excluded_duplicate: 0,
  pending: 0,
  in_flight: 0,
  retry: 0,
  sent: 0,
  failed: 0,
  unknown: 0,
  skipped_no_consent: 0,
  ...c,
})

const campaign: TrackedCampaign = {
  id: '00000000-0000-4000-8000-000000000007',
  chat_id: 'c1',
  ordinal: 1,
  name: 'İstanbul sepet',
  status: 'sending',
  updated_at: '2026-10-27T10:00:00Z',
  approved_at: '2026-10-27T09:00:00Z',
  last_sent_at: '2026-10-27T10:00:00Z',
  version: 3,
  llm_provider: 'mock',
  channels: {
    sms: counts({ sent: 120, failed: 2, unknown: 1, pending: 40, in_flight: 2, retry: 10, excluded_consent: 79 }),
    email: counts({ sent: 130, pending: 65, excluded_duplicate: 3 }),
  },
}

describe('campaignTotals', () => {
  it('sums a campaign: what it targets, what is done, what waits', () => {
    expect(campaignTotals(campaign)).toEqual({
      target: 370, // every delivery queued at approval
      sent: 250,
      failed: 2,
      unknown: 1,
      skipped: 0,
      queued: 117,
      done: 253,
      excludedConsent: 79,
      excludedDuplicate: 3,
      progress: 253 / 370,
    })
  })

  it('narrows to one channel', () => {
    const sms = campaignTotals(campaign, 'sms')
    expect([sms.target, sms.sent, sms.queued, sms.excludedDuplicate]).toEqual([175, 120, 52, 0])
  })
})

describe('foldCities', () => {
  const cities: AnalyticsCities = {
    top: [1, 2, 3, 4, 5, 6, 7].map((i) => ({ geoname_id: i, name: `Şehir ${i}`, sms: 10 - i, email: 0, total: 10 - i })),
    other: { cities: 3, total: 4 },
    unknown: { total: 2, sms: 2, email: 0 },
  }

  it('keeps the first n and folds the rest into the other cities', () => {
    const f = foldCities(cities, 5)
    expect(f.top.map((c) => c.geoname_id)).toEqual([1, 2, 3, 4, 5])
    // the 3 cities already folded (total 4) plus cities 6 and 7 (totals 4 and 3)
    expect(f.other).toEqual({ cities: 5, total: 4 + 4 + 3 })
    expect(f.unknown).toEqual(cities.unknown)
  })

  it('leaves a short list alone', () => {
    expect(foldCities(cities, 10)).toEqual(cities)
  })
})
