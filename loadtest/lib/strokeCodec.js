// Binary stroke frames for k6 (ADR 0025): doodle.game.v1.StrokeFrame, the same hand-written
// codec as web/src/game/strokeCodec.ts (whose tests check it against the game server's bytes),
// in plain JavaScript. k6 loads no npm packages, and the strings here (ids, colors) are ASCII.
export const STROKES_SUBPROTOCOL = 'doodle.strokes.pb'

function varint(out, n) {
  while (n > 127) {
    out.push((n % 128) | 128)
    n = Math.floor(n / 128)
  }
  out.push(n)
}

function field(out, number, bytes) {
  varint(out, number * 8 + 2)
  varint(out, bytes.length)
  for (const b of bytes) out.push(b)
}

function ascii(s) {
  const out = []
  for (let i = 0; i < s.length; i++) out.push(s.charCodeAt(i) & 0x7f)
  return out
}

/** A drawer's batch ({stroke_id, points, color, width, done}) as a binary frame (ArrayBuffer). */
export function encodeStrokeBatch(m) {
  const points = []
  for (const [x, y, t] of m.points) {
    varint(points, x)
    varint(points, y)
    varint(points, t)
  }
  const batch = []
  field(batch, 1, ascii(m.stroke_id))
  field(batch, 2, points)
  field(batch, 3, ascii(m.color))
  varint(batch, 4 * 8)
  varint(batch, m.width)
  if (m.done) {
    varint(batch, 5 * 8)
    varint(batch, 1)
  }
  const frame = []
  field(frame, 1, batch)
  return new Uint8Array(frame).buffer
}

function reader(bytes) {
  let pos = 0
  const r = {
    done: () => pos >= bytes.length,
    varint() {
      let n = 0
      let scale = 1
      for (;;) {
        if (pos >= bytes.length) throw new Error('truncated')
        const b = bytes[pos++]
        n += (b & 127) * scale
        if (b < 128) return n
        scale *= 128
      }
    },
    bytes() {
      const len = r.varint()
      const out = bytes.subarray(pos, pos + len)
      pos += len
      return out
    },
    skip(wire) {
      if (wire === 0) r.varint()
      else if (wire === 2) r.bytes()
      else if (wire === 1) pos += 8
      else if (wire === 5) pos += 4
      else throw new Error('wire type ' + wire)
    },
  }
  return r
}

const text = (b) => String.fromCharCode.apply(null, Array.from(b))

/** A server's binary frame (ArrayBuffer) as the event {seq, type: 'stroke_batch', data}, or null. */
export function decodeStrokeEvent(buf) {
  const frame = reader(new Uint8Array(buf))
  let event = null
  while (!frame.done()) {
    const key = frame.varint()
    if (key === 2 * 8 + 2) event = frame.bytes()
    else frame.skip(key % 8)
  }
  if (!event) return null
  const r = reader(event)
  const data = { user_id: '', stroke_id: '', points: [], color: '', width: 0, done: false }
  const flat = []
  let seq = 0
  while (!r.done()) {
    const key = r.varint()
    const f = Math.floor(key / 8)
    const wire = key % 8
    if (f === 1 && wire === 0) seq = r.varint()
    else if (f === 2 && wire === 2) data.user_id = text(r.bytes())
    else if (f === 3 && wire === 2) data.stroke_id = text(r.bytes())
    else if (f === 4 && wire === 2) {
      const p = reader(r.bytes())
      while (!p.done()) flat.push(p.varint())
    } else if (f === 5 && wire === 2) data.color = text(r.bytes())
    else if (f === 6 && wire === 0) data.width = r.varint()
    else if (f === 7 && wire === 0) data.done = r.varint() !== 0
    else r.skip(wire)
  }
  for (let i = 0; i + 2 < flat.length; i += 3) data.points.push([flat[i], flat[i + 1], flat[i + 2]])
  return { seq, type: 'stroke_batch', data }
}
