import { describe, expect, it } from 'vitest'
import type { Replay, ReplayStroke } from '../gallery.ts'
import { eventsUntil, ReplayPainter, timeline } from './replay.ts'

function stroke(points: [number, number, number][], receivedMs: number, undone = false): ReplayStroke {
  return { user_id: 'u_ada', color: '#1971c2', width: 60, undone, server_received_ms: receivedMs, points }
}

function replay(strokes: ReplayStroke[]): Replay {
  return { drawing_id: 'd', schema_version: 1, canvas_width: 800, canvas_height: 600, round_started_at: null, strokes }
}

describe('timeline', () => {
  it('places points at the stroke start plus their offset', () => {
    // The server got the last batch at 1000 ms; the stroke took 300 ms, so it began at 700.
    const tl = timeline(replay([stroke([[0, 0, 0], [10, 10, 100], [20, 20, 300]], 1000)]), 10_000)

    expect(tl.events.map((e) => e.at)).toEqual([700, 800, 1000])
    expect(tl.duration).toBe(1000)
  })

  it('squeezes long pauses, including the wait before the first stroke', () => {
    const tl = timeline(
      replay([stroke([[0, 0, 0], [1, 1, 200]], 5200), stroke([[2, 2, 0], [3, 3, 100]], 9100)]),
      1000,
    )

    // Raw times 5000, 5200, 9000, 9100: the 5 s and 3.8 s gaps become 1 s each.
    expect(tl.events.map((e) => e.at)).toEqual([1000, 1200, 2200, 2300])
    expect(tl.duration).toBe(2300)
  })

  it('leaves out undone and empty strokes', () => {
    const tl = timeline(
      replay([
        stroke([[0, 0, 0]], 100),
        stroke([[5, 5, 0], [6, 6, 50]], 300, true),
        stroke([], 400),
        stroke([[9, 9, 0]], 500),
      ]),
    )

    expect(tl.strokes).toHaveLength(2)
    expect(tl.events.map((e) => [e.stroke, e.point])).toEqual([
      [0, 0],
      [1, 0],
    ])
  })

  it('interleaves two drawers by time', () => {
    const tl = timeline(
      replay([stroke([[0, 0, 0], [1, 1, 400]], 400), stroke([[5, 5, 0], [6, 6, 400]], 600)]),
      10_000,
    )

    expect(tl.events.map((e) => [e.at, e.stroke])).toEqual([
      [0, 0],
      [200, 1],
      [400, 0],
      [600, 1],
    ])
  })

  it('never starts a stroke before the round', () => {
    // A client clock that ran fast: the stroke claims to be longer than the round so far.
    const tl = timeline(replay([stroke([[0, 0, 0], [1, 1, 900]], 500)]), 10_000)

    expect(tl.events.map((e) => e.at)).toEqual([0, 900])
  })

  it('handles an empty replay', () => {
    expect(timeline(replay([]))).toEqual({ strokes: [], events: [], duration: 0 })
  })
})

describe('eventsUntil', () => {
  const events = [0, 100, 100, 250].map((at, i) => ({ at, stroke: 0, point: i }))

  it('counts events at or before a time', () => {
    expect([-1, 0, 99, 100, 249, 250, 9999].map((ms) => eventsUntil(events, ms))).toEqual([0, 1, 1, 3, 3, 4, 4])
  })
})

function fakeContext() {
  const calls: string[] = []
  const ctx = {
    clearRect: () => calls.push('clear'),
    beginPath: () => {},
    moveTo: (x: number, y: number) => calls.push(`move ${x},${y}`),
    lineTo: (x: number, y: number) => calls.push(`line ${x},${y}`),
    arc: (x: number, y: number) => calls.push(`dot ${x},${y}`),
    stroke: () => {},
    fill: () => {},
  }
  return { ctx: ctx as unknown as CanvasRenderingContext2D, calls }
}

describe('ReplayPainter', () => {
  const tl = timeline(replay([stroke([[0, 0, 0], [5000, 0, 100], [10000, 10000, 200]], 200)]))

  it('draws new points as segments joined to the stroke so far', () => {
    const { ctx, calls } = fakeContext()
    const p = new ReplayPainter()

    p.paint(ctx, tl, 1, 100, 100)
    p.paint(ctx, tl, 3, 100, 100)

    expect(calls).toEqual(['dot 0,0', 'move 0,0', 'line 50,0', 'move 50,0', 'line 100,100'])
  })

  it('repaints from scratch when it goes back', () => {
    const { ctx, calls } = fakeContext()
    const p = new ReplayPainter()
    p.paint(ctx, tl, 3, 100, 100)
    calls.length = 0

    p.paint(ctx, tl, 1, 100, 100)

    expect(calls).toEqual(['clear', 'dot 0,0'])
  })
})
