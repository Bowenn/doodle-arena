import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { strokeBatchMs } from '../config.ts'
import type { StrokeBatch } from './protocol.ts'
import { newStrokeId, normalize, StrokeBatcher } from './strokes.ts'

describe('StrokeBatcher', () => {
  let sent: StrokeBatch[]
  let clock: number
  let batcher: StrokeBatcher

  beforeEach(() => {
    vi.useFakeTimers()
    sent = []
    clock = 1000
    batcher = new StrokeBatcher((b) => sent.push(b), {
      intervalMs: 50,
      now: () => clock,
      newId: () => 's1',
    })
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('sends the points of each interval as one batch', () => {
    batcher.begin(10, 10, '#000000', 40)
    clock += 10
    batcher.add(20, 20)
    expect(sent).toHaveLength(0) // nothing before the interval

    vi.advanceTimersByTime(50)
    expect(sent).toEqual([
      { stroke_id: 's1', points: [[10, 10, 0], [20, 20, 10]], color: '#000000', width: 40, done: false },
    ])

    vi.advanceTimersByTime(200)
    expect(sent).toHaveLength(1) // no empty batches
  })

  it('ends a stroke with done = true and the remaining points', () => {
    batcher.begin(10, 10, '#000000', 40)
    vi.advanceTimersByTime(50)
    clock += 70
    batcher.add(30, 30)
    batcher.end()

    expect(sent[1]).toMatchObject({ points: [[30, 30, 70]], done: true })
    vi.advanceTimersByTime(500)
    expect(sent).toHaveLength(2) // the timer stopped
  })

  it('repeats the last point when the stroke ends between batches', () => {
    batcher.begin(10, 10, '#000000', 40)
    vi.advanceTimersByTime(50)
    batcher.end()
    expect(sent[1]).toMatchObject({ points: [[10, 10, 0]], done: true })
  })

  it('flushes at once when a batch reaches 512 points', () => {
    batcher.begin(0, 0, '#000000', 40)
    for (let i = 1; i <= 600; i++) batcher.add(i, i)

    expect(sent).toHaveLength(1)
    expect(sent[0].points).toHaveLength(512)
    batcher.end()
    expect(sent[1].points).toHaveLength(601 - 512)
    expect(sent.every((b) => b.points.length >= 1 && b.points.length <= 512)).toBe(true)
  })

  it('drops repeated points', () => {
    batcher.begin(5, 5, '#000000', 40)
    batcher.add(5, 5)
    batcher.add(5, 5)
    batcher.add(6, 5)
    batcher.end()
    expect(sent[0].points.map(([x, y]) => [x, y])).toEqual([[5, 5], [6, 5]])
  })

  it('sends nothing more after cancel', () => {
    batcher.begin(5, 5, '#000000', 40)
    batcher.cancel()
    batcher.add(6, 6)
    batcher.end()
    vi.advanceTimersByTime(500)
    expect(sent).toHaveLength(0)
    expect(batcher.active).toBe(false)
  })
})

describe('normalize', () => {
  it('maps element coordinates to 0..10000 and clamps outside points', () => {
    expect(normalize(0, 0, 800, 600)).toEqual([0, 0])
    expect(normalize(400, 300, 800, 600)).toEqual([5000, 5000])
    expect(normalize(800, 600, 800, 600)).toEqual([10000, 10000])
    expect(normalize(-20, 700, 800, 600)).toEqual([0, 10000])
  })
})

describe('newStrokeId', () => {
  it('matches the server’s stroke id pattern', () => {
    expect(newStrokeId()).toMatch(/^[A-Za-z0-9_-]{1,32}$/)
  })
})

describe('strokeBatchMs', () => {
  it('defaults to 50 ms and accepts a clamped ?batch_ms= override', () => {
    expect(strokeBatchMs('')).toBe(50)
    expect(strokeBatchMs('?batch_ms=100')).toBe(100)
    expect(strokeBatchMs('?batch_ms=1')).toBe(10)
    expect(strokeBatchMs('?batch_ms=99999')).toBe(1000)
    expect(strokeBatchMs('?batch_ms=abc')).toBe(50)
  })
})
