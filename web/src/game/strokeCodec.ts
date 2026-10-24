// Binary stroke frames (ADR 0025): doodle.game.v1.StrokeFrame from proto/doodle/game/v1/ws.proto,
// written by hand for its two messages instead of a protobuf runtime, to keep the bundle small.
// Tests check it against bytes the game server produces (testdata/stroke-frames.json).
//
//   StrokeFrame      { oneof frame { StrokeBatchIn batch = 1; StrokeBatchEvent event = 2; } }
//   StrokeBatchIn    { string stroke_id = 1; repeated uint32 points = 2 (packed); string color = 3;
//                      uint32 width = 4; bool done = 5; }
//   StrokeBatchEvent { int64 seq = 1; string user_id = 2; string stroke_id = 3;
//                      repeated uint32 points = 4 (packed); string color = 5; uint32 width = 6; bool done = 7; }
import type { CanvasBatch, Point, StrokeBatch } from './protocol.ts'

/** The WebSocket subprotocol that turns binary stroke frames on for a socket. */
export const STROKES_SUBPROTOCOL = 'doodle.strokes.pb'

const VARINT = 0
const I64 = 1
const LEN = 2
const I32 = 5

const utf8 = new TextEncoder()
const fromUtf8 = new TextDecoder('utf-8', { fatal: true })

class Writer {
  private bytes: number[] = []

  varint(n: number) {
    // Values here are non-negative and below 2^53: plain arithmetic, no 32-bit shifts.
    while (n > 127) {
      this.bytes.push((n % 128) | 128)
      n = Math.floor(n / 128)
    }
    this.bytes.push(n)
  }

  tag(field: number, wire: number) {
    this.varint(field * 8 + wire)
  }

  bytesField(field: number, b: Uint8Array) {
    this.tag(field, LEN)
    this.varint(b.length)
    for (const x of b) this.bytes.push(x)
  }

  finish(): Uint8Array<ArrayBuffer> {
    return Uint8Array.from(this.bytes)
  }
}

/** A drawer's batch as a binary StrokeFrame. */
export function encodeStrokeBatch(m: StrokeBatch): Uint8Array<ArrayBuffer> {
  const points = new Writer()
  for (const [x, y, t] of m.points) {
    points.varint(x)
    points.varint(y)
    points.varint(t)
  }
  const batch = new Writer()
  batch.bytesField(1, utf8.encode(m.stroke_id))
  batch.bytesField(2, points.finish())
  batch.bytesField(3, utf8.encode(m.color))
  if (m.width) {
    batch.tag(4, VARINT)
    batch.varint(m.width)
  }
  if (m.done) {
    batch.tag(5, VARINT)
    batch.varint(1)
  }
  const frame = new Writer()
  frame.bytesField(1, batch.finish())
  return frame.finish()
}

class Reader {
  private pos = 0
  private readonly buf: Uint8Array

  constructor(buf: Uint8Array) {
    this.buf = buf
  }

  done(): boolean {
    return this.pos >= this.buf.length
  }

  varint(): number {
    let n = 0
    let scale = 1
    for (;;) {
      if (this.pos >= this.buf.length) throw new Error('truncated varint')
      const b = this.buf[this.pos++]
      n += (b & 127) * scale
      if (b < 128) return n
      scale *= 128
      if (scale > 2 ** 63) throw new Error('varint too long')
    }
  }

  bytes(): Uint8Array {
    const len = this.varint()
    if (this.pos + len > this.buf.length) throw new Error('truncated field')
    const out = this.buf.subarray(this.pos, this.pos + len)
    this.pos += len
    return out
  }

  skip(wire: number) {
    if (wire === VARINT) this.varint()
    else if (wire === LEN) this.bytes()
    else if (wire === I64) this.pos += 8
    else if (wire === I32) this.pos += 4
    else throw new Error(`wire type ${wire}`)
  }
}

function readPoints(r: Reader, wire: number, out: number[]) {
  if (wire === LEN) {
    const packed = new Reader(r.bytes())
    while (!packed.done()) out.push(packed.varint())
  } else {
    out.push(r.varint()) // an unpacked element
  }
}

function triples(flat: number[]): Point[] {
  if (flat.length % 3 !== 0) throw new Error('points are x, y, t_ms triples')
  const points: Point[] = []
  for (let i = 0; i < flat.length; i += 3) points.push([flat[i], flat[i + 1], flat[i + 2]])
  return points
}

/** A server's binary frame as the same stroke_batch event a JSON frame would carry, or null if
 * it isn't one (a frame this client can't read is ignored, like a malformed JSON message). */
export function decodeStrokeEvent(buf: Uint8Array): { seq: number; type: 'stroke_batch'; data: CanvasBatch } | null {
  try {
    const frame = new Reader(buf)
    let event: Uint8Array | null = null
    while (!frame.done()) {
      const key = frame.varint()
      if (key === 2 * 8 + LEN) event = frame.bytes()
      else frame.skip(key % 8)
    }
    if (!event) return null
    const r = new Reader(event)
    const flat: number[] = []
    const ev = { seq: 0, user_id: '', stroke_id: '', color: '', width: 0, done: false }
    while (!r.done()) {
      const key = r.varint()
      const field = Math.floor(key / 8)
      const wire = key % 8
      if (field === 1 && wire === VARINT) ev.seq = r.varint()
      else if (field === 2 && wire === LEN) ev.user_id = fromUtf8.decode(r.bytes())
      else if (field === 3 && wire === LEN) ev.stroke_id = fromUtf8.decode(r.bytes())
      else if (field === 4) readPoints(r, wire, flat)
      else if (field === 5 && wire === LEN) ev.color = fromUtf8.decode(r.bytes())
      else if (field === 6 && wire === VARINT) ev.width = r.varint()
      else if (field === 7 && wire === VARINT) ev.done = r.varint() !== 0
      else r.skip(wire)
    }
    const data: CanvasBatch = {
      user_id: ev.user_id,
      stroke_id: ev.stroke_id,
      points: triples(flat),
      color: ev.color,
      width: ev.width,
      done: ev.done,
    }
    return { seq: ev.seq, type: 'stroke_batch', data }
  } catch {
    return null
  }
}
