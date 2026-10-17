// The drawing surface. Two stacked canvases:
// - the base layer paints only what the server broadcast (stroke_batch events), so every
//   player sees the same picture;
// - the overlay paints the drawer's own stroke right away, before the server echoes it, so
//   drawing doesn't lag by a round trip. A local stroke disappears once the server's copy
//   (its done batch) has arrived, or when the server resets the state.

import { useEffect, useRef, useState, type PointerEvent } from 'react'
import { strokeBatchMs } from '../config.ts'
import type { CanvasBatch, ClientMessage, Point } from '../game/protocol.ts'
import { CanvasPainter, drawPoints } from '../game/render.ts'
import { normalize, StrokeBatcher } from '../game/strokes.ts'

const COLORS = ['#000000', '#e03131', '#f08c00', '#2f9e44', '#1971c2', '#7048e8', '#8b5a2b', '#ffffff']
/** Widths in normalized units (1/10000 of the canvas width). */
const WIDTHS = [20, 60, 150]

type LocalStroke = { color: string; width: number; points: Point[] }

function paintLocal(c: HTMLCanvasElement | null, strokes: Map<string, LocalStroke>) {
  const ctx = c?.getContext('2d')
  if (!c || !ctx) return
  ctx.clearRect(0, 0, c.width, c.height)
  for (const s of strokes.values()) drawPoints(ctx, s, null, c.width, c.height)
}

type Props = {
  strokes: readonly CanvasBatch[]
  me: string
  canDraw: boolean
  send: (msg: ClientMessage) => boolean
  /** Changes on every welcome: local strokes from before it are dropped. */
  resetKey: number
}

export function Canvas({ strokes, me, canDraw, send, resetKey }: Props) {
  const wrap = useRef<HTMLDivElement>(null)
  const base = useRef<HTMLCanvasElement>(null)
  const overlay = useRef<HTMLCanvasElement>(null)
  const painter = useRef(new CanvasPainter())
  const local = useRef(new Map<string, LocalStroke>())
  const current = useRef<string | null>(null)
  const [size, setSize] = useState({ w: 0, h: 0 })
  const [color, setColor] = useState(COLORS[0])
  const [width, setWidth] = useState(WIDTHS[1])

  const batcher = useRef<StrokeBatcher | null>(null)
  const sendRef = useRef(send)
  useEffect(() => {
    sendRef.current = send
  }, [send])
  useEffect(() => {
    const b = new StrokeBatcher(
      (data) => {
        if (!sendRef.current({ type: 'stroke_batch', data })) {
          // The socket is down: this stroke can't reach the server, so drop it.
          batcher.current?.cancel()
          local.current.delete(data.stroke_id)
          current.current = null
          paintLocal(overlay.current, local.current)
        }
      },
      { intervalMs: strokeBatchMs() },
    )
    batcher.current = b
    return () => b.cancel()
  }, [])

  // Size the backing stores to the element and the screen's pixel ratio.
  useEffect(() => {
    const el = wrap.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(() => {
      const dpr = window.devicePixelRatio || 1
      const w = Math.round(el.clientWidth * dpr)
      const h = Math.round(el.clientHeight * dpr)
      for (const c of [base.current, overlay.current]) {
        if (c) {
          c.width = w
          c.height = h
        }
      }
      painter.current.reset()
      setSize({ w, h })
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  // Paint the server's strokes, and retire local strokes the server now has.
  useEffect(() => {
    const ctx = base.current?.getContext('2d')
    if (ctx && size.w > 0) painter.current.paint(ctx, strokes, size.w, size.h)
    let changed = false
    for (const id of local.current.keys()) {
      if (id !== current.current && strokes.some((b) => b.done && b.stroke_id === id && b.user_id === me)) {
        local.current.delete(id)
        changed = true
      }
    }
    if (changed) paintLocal(overlay.current, local.current)
  }, [strokes, size, me])

  // Stop drawing when the turn ends, and forget local strokes after a welcome.
  useEffect(() => {
    if (!canDraw) batcher.current?.cancel()
    local.current.clear()
    current.current = null
    paintLocal(overlay.current, local.current)
  }, [canDraw, resetKey])

  function point(e: { clientX: number; clientY: number }): [number, number] {
    const r = overlay.current!.getBoundingClientRect()
    return normalize(e.clientX - r.left, e.clientY - r.top, r.width, r.height)
  }

  function onDown(e: PointerEvent<HTMLCanvasElement>) {
    if (!canDraw || e.button !== 0 || !batcher.current) return
    e.currentTarget.setPointerCapture(e.pointerId)
    const [x, y] = point(e)
    const id = batcher.current.begin(x, y, color, width)
    current.current = id
    local.current.set(id, { color, width, points: [[x, y, 0]] })
    paintLocal(overlay.current, local.current)
  }

  function onMove(e: PointerEvent<HTMLCanvasElement>) {
    const id = current.current
    const s = id && local.current.get(id)
    if (!s || !batcher.current?.active) return
    // Coalesced events carry the points the browser merged into this one: smoother lines.
    const events = e.nativeEvent.getCoalescedEvents?.() ?? [e.nativeEvent]
    for (const ev of events.length ? events : [e.nativeEvent]) {
      const [x, y] = point(ev)
      batcher.current.add(x, y)
      s.points.push([x, y, 0])
    }
    paintLocal(overlay.current, local.current)
  }

  function onUp() {
    if (!current.current) return
    batcher.current?.end()
    current.current = null
  }

  function undo() {
    // The newest stroke of mine the server has; the server checks that it is mine.
    for (let i = strokes.length - 1; i >= 0; i--) {
      if (strokes[i].user_id === me) {
        send({ type: 'undo', data: { stroke_id: strokes[i].stroke_id } })
        return
      }
    }
  }

  return (
    <div className="canvas-area">
      <div className="canvas-wrap" ref={wrap}>
        <canvas ref={base} className="canvas" aria-label="Drawing" role="img" />
        <canvas
          ref={overlay}
          className={canDraw ? 'canvas overlay can-draw' : 'canvas overlay'}
          onPointerDown={onDown}
          onPointerMove={onMove}
          onPointerUp={onUp}
          onPointerCancel={onUp}
        />
      </div>
      {canDraw && (
        <div className="toolbar" role="toolbar" aria-label="Drawing tools">
          {COLORS.map((c) => (
            <button
              key={c}
              type="button"
              className={c === color ? 'swatch selected' : 'swatch'}
              style={{ background: c }}
              aria-label={`Color ${c}`}
              aria-pressed={c === color}
              onClick={() => setColor(c)}
            />
          ))}
          {WIDTHS.map((w, i) => (
            <button
              key={w}
              type="button"
              className={w === width ? 'selected' : ''}
              aria-pressed={w === width}
              onClick={() => setWidth(w)}
            >
              {['Thin', 'Medium', 'Thick'][i]}
            </button>
          ))}
          <button type="button" onClick={undo}>
            Undo
          </button>
        </div>
      )}
    </div>
  )
}
