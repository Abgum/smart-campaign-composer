import { describe, expect, it } from 'vitest'
import { localInputValue, scheduledLater, toUTC } from './schedule'

describe('schedule', () => {
  it('turns the date and time picked on this device into a UTC instant', () => {
    expect(toUTC('2099-11-03T14:30')).toBe(new Date(2099, 10, 3, 14, 30).toISOString())
    expect(toUTC('2099-11-03T14:30')).toMatch(/Z$/)
    expect(toUTC('')).toBeNull()
    expect(toUTC('nope')).toBeNull()
  })

  it('writes an instant as a date-time input value in local time', () => {
    expect(localInputValue(new Date(2099, 10, 3, 9, 5))).toBe('2099-11-03T09:05')
  })

  it('tells a campaign whose sending waits for its time', () => {
    const now = Date.parse('2026-11-01T10:00:00Z')
    expect(scheduledLater('2026-11-01T10:00:01Z', now)).toBe(true)
    expect(scheduledLater('2026-11-01T10:00:00Z', now)).toBe(false)
    expect(scheduledLater(null, now)).toBe(false)
    expect(scheduledLater(undefined, now)).toBe(false)
  })
})
