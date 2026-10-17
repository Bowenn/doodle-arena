import { describe, expect, it } from 'vitest'
import { isoWeek, shiftWeek } from './weeks.ts'

describe('ISO weeks', () => {
  it.each([
    ['2026-10-16T12:00:00Z', '2026-W42'],
    ['2026-10-18T23:59:59Z', '2026-W42'], // Sunday night, UTC
    ['2026-10-19T00:00:00Z', '2026-W43'], // Monday midnight starts a new week
    ['2026-10-19T01:00:00+02:00', '2026-W42'], // still Sunday in UTC
    ['2026-12-31T12:00:00Z', '2026-W53'],
    ['2027-01-01T12:00:00Z', '2026-W53'],
    ['2027-01-04T00:00:00Z', '2027-W01'],
    ['2025-12-29T00:00:00Z', '2026-W01'],
  ])('%s is in %s', (at, week) => {
    expect(isoWeek(new Date(at))).toBe(week)
  })

  it.each([
    ['2026-W42', -1, '2026-W41'],
    ['2026-W42', 1, '2026-W43'],
    ['2026-W53', 1, '2027-W01'],
    ['2027-W01', -1, '2026-W53'],
    ['2026-W01', -1, '2025-W52'],
  ])('%s shifted by %i is %s', (week, by, want) => {
    expect(shiftWeek(week, by)).toBe(want)
  })
})
