// @vitest-environment node
import { describe, expect, it } from 'vitest'
import { analyticsQuery, defaultFilters, filtersQuery, parseFilters } from './filters'

const campaignId = '00000000-0000-4000-8000-000000000005'

describe('overview filters in the URL', () => {
  it('defaults to the last 7 days, every channel and campaign', () => {
    expect(parseFilters({})).toEqual({ range: '7d', channel: '', campaign: null })
  })

  it('reads the range, channel and campaign', () => {
    expect(parseFilters({ range: '30d', channel: 'sms', campaign: campaignId })).toEqual({ range: '30d', channel: 'sms', campaign: campaignId })
  })

  it('ignores values it does not know', () => {
    expect(parseFilters({ range: 'tomorrow', channel: 'fax', campaign: 'abc' })).toEqual(defaultFilters)
    expect(parseFilters({ campaign: '-3' }).campaign).toBeNull()
    expect(parseFilters({ campaign: '5' }).campaign).toBeNull()
  })

  it('reads one day, and only a real one', () => {
    expect(parseFilters({ range: 'day', day: '2026-10-25' })).toEqual({ range: 'day', day: '2026-10-25', channel: '', campaign: null })
    expect(parseFilters({ range: 'day' })).toEqual(defaultFilters)
    expect(parseFilters({ range: 'day', day: '2026-02-30' })).toEqual(defaultFilters)
    expect(parseFilters({ range: 'day', day: '25.10.2026' })).toEqual(defaultFilters)
  })

  it('puts one day in the URL, and drops a day the range does not use', () => {
    expect(filtersQuery({ range: 'day', day: '2026-10-25', channel: '', campaign: null })).toEqual({ range: 'day', day: '2026-10-25' })
    expect(filtersQuery({ range: '30d', day: '2026-10-25', channel: '', campaign: null })).toEqual({ range: '30d' })
  })

  it('keeps defaults out of the URL', () => {
    expect(filtersQuery(defaultFilters)).toEqual({})
    expect(filtersQuery({ range: 'today', channel: 'email', campaign: campaignId })).toEqual({ range: 'today', channel: 'email', campaign: campaignId })
  })

  it('asks the API for the same slice, with the same names', () => {
    expect(analyticsQuery(defaultFilters, 10)).toBe('range=7d&cities=10')
    expect(analyticsQuery({ range: 'today', channel: 'email', campaign: campaignId }, 5)).toBe(`range=today&cities=5&channel=email&campaign_id=${campaignId}`)
    expect(analyticsQuery({ range: 'all', channel: '', campaign: null }, 10)).toBe('range=all&cities=10')
    expect(analyticsQuery({ range: 'day', day: '2026-10-25', channel: 'sms', campaign: null }, 10)).toBe('range=day&day=2026-10-25&cities=10&channel=sms')
  })
})
