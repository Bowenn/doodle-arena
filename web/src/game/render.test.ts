import { describe, expect, it } from 'vitest'
import type { CanvasBatch, Point } from './protocol.ts'
import { CanvasPainter } from './render.ts'

/** Records the drawing calls a painter makes. */
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

function batch(id: string, points: Point[], user = 'a'): CanvasBatch {
  return { user_id: user, stroke_id: id, points, color: '#000000', width: 100, done: false }
}

describe('CanvasPainter', () => {
  it('scales normalized points to the canvas', () => {
    const { ctx, calls } = fakeContext()
    new CanvasPainter().paint(ctx, [batch('s1', [[0, 0, 0], [5000, 10000, 10]])], 200, 100)
    expect(calls).toEqual(['clear', 'move 0,0', 'line 100,100'])
  })

  it('draws only new batches, continuing each stroke from its last point', () => {
    const { ctx, calls } = fakeContext()
    const p = new CanvasPainter()
    const first = [batch('s1', [[0, 0, 0], [1000, 0, 10]])]
    p.paint(ctx, first, 100, 100)
    calls.length = 0

    p.paint(ctx, [...first, batch('s1', [[2000, 0, 20]])], 100, 100)
    expect(calls).toEqual(['move 10,0', 'line 20,0']) // no clear, joined to the previous batch
  })

  it('repaints from scratch when a batch was removed', () => {
    const { ctx, calls } = fakeContext()
    const p = new CanvasPainter()
    const a = batch('s1', [[0, 0, 0]])
    const b = batch('s2', [[1000, 1000, 0]])
    p.paint(ctx, [a, b], 100, 100)
    calls.length = 0

    p.paint(ctx, [b], 100, 100) // s1 was undone
    expect(calls).toEqual(['clear', 'dot 10,10'])
  })

  it('continues each drawer’s stroke separately when two drawers use the same id', () => {
    const { ctx, calls } = fakeContext()
    const p = new CanvasPainter()
    const first = [batch('s1', [[0, 0, 0], [1000, 0, 10]], 'a'), batch('s1', [[0, 5000, 0], [1000, 5000, 10]], 'b')]
    p.paint(ctx, first, 100, 100)
    calls.length = 0

    p.paint(ctx, [...first, batch('s1', [[2000, 0, 20]], 'a'), batch('s1', [[2000, 5000, 20]], 'b')], 100, 100)
    expect(calls).toEqual(['move 10,0', 'line 20,0', 'move 10,50', 'line 20,50'])
  })

  it('repaints when the same number of batches is a different list', () => {
    const { ctx, calls } = fakeContext()
    const p = new CanvasPainter()
    p.paint(ctx, [batch('s1', [[0, 0, 0]])], 100, 100)
    calls.length = 0
    p.paint(ctx, [batch('s9', [[5000, 5000, 0]])], 100, 100)
    expect(calls[0]).toBe('clear')
  })
})
