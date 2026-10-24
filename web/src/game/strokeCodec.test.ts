import { describe, expect, it } from 'vitest'
import type { Point } from './protocol.ts'
import { decodeStrokeEvent, encodeStrokeBatch } from './strokeCodec.ts'
// Bytes the game server's Go code produced (internal/protocol TestWebStrokeFixturesMatchGo).
import fixtures from './testdata/stroke-frames.json'

const hex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')
const bytes = (h: string) => Uint8Array.from(h.match(/../g) ?? [], (x) => parseInt(x, 16))

describe('strokeCodec', () => {
  it('encodes a batch to the same bytes as the game server', () => {
    const b = fixtures.batch
    const out = encodeStrokeBatch({
      stroke_id: b.stroke_id,
      points: b.points as Point[],
      color: b.color,
      width: b.width,
      done: b.done,
    })
    expect(hex(out)).toBe(b.hex)
  })

  it("decodes the game server's event frame to the JSON event's shape", () => {
    const e = fixtures.event
    expect(decodeStrokeEvent(bytes(e.hex))).toEqual({
      seq: e.seq,
      type: 'stroke_batch',
      data: { user_id: e.user_id, stroke_id: e.stroke_id, points: e.points, color: e.color, width: e.width, done: e.done },
    })
  })

  it('packs points as varints across their byte boundaries', () => {
    const points: Point[] = [
      [0, 127, 128],
      [16383, 16384, 600000],
      [10000, 1, 2],
    ]
    const batch = encodeStrokeBatch({ stroke_id: 's1', points, color: '#000000', width: 1, done: false })
    // 0, 127 | 128 | 16383 | 16384 | 600000 | 10000 | 1, 2: one, two or three bytes each.
    expect(hex(batch)).toContain('12' + '10' + '007f' + '8001' + 'ff7f' + '808001' + 'c0cf24' + '904e' + '0102')
    // A client's batch isn't an event: there is nothing for a client to decode.
    expect(decodeStrokeEvent(batch)).toBeNull()
  })

  it('ignores frames it cannot read', () => {
    expect(decodeStrokeEvent(new Uint8Array([0xff, 0xff, 0xff]))).toBeNull()
    expect(decodeStrokeEvent(new Uint8Array([]))).toBeNull()
    // An event whose points aren't triples.
    expect(decodeStrokeEvent(Uint8Array.from([0x12, 0x04, 0x22, 0x02, 0x01, 0x02]))).toBeNull()
  })
})
