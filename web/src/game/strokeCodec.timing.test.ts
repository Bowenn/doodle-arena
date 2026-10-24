// The client's side of ADR 0025, timed in V8 (as in Chrome): the game server's stroke event
// (testdata, 3 points) received as a binary frame or as JSON, and a batch sent either way.
// Opt-in, since timings aren't assertions: BENCH=1 npx vitest run src/game/strokeCodec.timing.test.ts
import { describe, expect, it } from 'vitest'
import type { Point } from './protocol.ts'
import { decodeStrokeEvent, encodeStrokeBatch } from './strokeCodec.ts'
import fixtures from './testdata/stroke-frames.json'

const e = fixtures.event
const data = { user_id: e.user_id, stroke_id: e.stroke_id, points: e.points as Point[], color: e.color, width: e.width, done: e.done }
const json = JSON.stringify({ type: 'event', data: { seq: e.seq, type: 'stroke_batch', data } })
const binary = Uint8Array.from(e.hex.match(/../g) ?? [], (x) => parseInt(x, 16))
const { user_id: _, ...batch } = data

function perCall(fn: () => unknown): number {
  for (let i = 0; i < 20_000; i++) fn() // warm up the JIT
  const n = 200_000
  const t = performance.now()
  for (let i = 0; i < n; i++) fn()
  return ((performance.now() - t) * 1000) / n // µs
}

describe.skipIf(!process.env.BENCH)('stroke codec timing', () => {
  it('prints µs per call', () => {
    const rows = {
      [`receive: JSON.parse (${json.length} B)`]: perCall(() => JSON.parse(json)),
      [`receive: decodeStrokeEvent (${binary.length} B)`]: perCall(() => decodeStrokeEvent(binary)),
      'send: JSON.stringify': perCall(() => JSON.stringify({ type: 'stroke_batch', data: batch })),
      'send: encodeStrokeBatch': perCall(() => encodeStrokeBatch(batch)),
    }
    for (const [name, us] of Object.entries(rows)) console.log(`${name}: ${us.toFixed(2)} µs`)
    expect(Object.values(rows).every((us) => us > 0)).toBe(true)
  })
})
