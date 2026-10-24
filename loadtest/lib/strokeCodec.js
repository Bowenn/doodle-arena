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

// k6 decodes every stroke frame every player receives, in its interpreter, so this reads the
// event in one pass without building a reader per field, and keeps only what the test uses:
// seq, user_id, stroke_id and the first and last points. The web app's decoder keeps them all.
function asciiText(b, from, to) {
  let out = ''
  for (let i = from; i < to; i++) out += String.fromCharCode(b[i])
  return out
}

/** A server's binary frame (ArrayBuffer) as the event {seq, type: 'stroke_batch', data}, or null.
 * data.points holds only the batch's first and last points. */
export function decodeStrokeEvent(buf) {
  const b = new Uint8Array(buf)
  let pos = 0
  const varint = () => {
    let n = 0
    let scale = 1
    for (;;) {
      const x = b[pos++]
      n += (x & 127) * scale
      if (x < 128) return n
      scale *= 128
    }
  }
  // StrokeFrame: find field 2 (event).
  let end = -1
  while (pos < b.length) {
    const key = varint()
    if (key === 2 * 8 + 2) {
      const len = varint()
      end = pos + len
      break
    }
    if (key % 8 === 2) pos += varint()
    else if (key % 8 === 0) varint()
    else return null
  }
  if (end < 0 || end > b.length) return null
  const data = { user_id: '', stroke_id: '', points: [], color: '', width: 0, done: false }
  let seq = 0
  while (pos < end) {
    const key = varint()
    const f = Math.floor(key / 8)
    const wire = key % 8
    if (wire === 0) {
      const v = varint()
      if (f === 1) seq = v
      else if (f === 6) data.width = v
      else if (f === 7) data.done = v !== 0
    } else if (wire === 2) {
      const len = varint()
      const stop = pos + len
      if (f === 2) data.user_id = asciiText(b, pos, stop)
      else if (f === 3) data.stroke_id = asciiText(b, pos, stop)
      else if (f === 5) data.color = asciiText(b, pos, stop)
      else if (f === 4) {
        let first = null
        let last = null
        while (pos < stop) {
          const p = [varint(), varint(), varint()]
          if (first === null) first = p
          last = p
        }
        if (first) data.points = first === last ? [first] : [first, last]
      }
      pos = stop
    } else {
      return null
    }
  }
  return { seq, type: 'stroke_batch', data }
}
