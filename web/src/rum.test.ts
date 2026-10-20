import { afterEach, describe, expect, it } from 'vitest'
import { attach, pageName, recordWsRtt, resetRumForTests } from './rum.ts'

describe('pageName', () => {
  it('names routes, never ids', () => {
    expect(pageName('/')).toBe('lobby')
    expect(pageName('/rooms/abc123xyz')).toBe('room')
    expect(pageName('/gallery')).toBe('gallery')
    expect(pageName('/my-drawings/')).toBe('my-drawings')
    expect(pageName('/leaderboard')).toBe('leaderboard')
    expect(pageName('/drawings/0192f3')).toBe('other')
    expect(pageName('/nope')).toBe('other')
  })
})

describe('recordWsRtt', () => {
  afterEach(resetRumForTests)

  it('queues round trips until the SDK attaches, then hands them over', () => {
    recordWsRtt(12)
    recordWsRtt(30)
    const got: number[] = []
    attach({ rtt: (ms) => got.push(ms) })
    recordWsRtt(7)
    expect(got).toEqual([12, 30, 7])
  })

  it('keeps at most 50 while the SDK never loads', () => {
    for (let i = 0; i < 80; i++) recordWsRtt(i)
    const got: number[] = []
    attach({ rtt: (ms) => got.push(ms) })
    expect(got).toHaveLength(50)
  })
})
