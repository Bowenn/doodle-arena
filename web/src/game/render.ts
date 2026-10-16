// Draws stroke batches on a 2D canvas. Coordinates and widths are normalized to 0..10000, so
// every screen shows the same picture at its own size.

import { MAX_COORD, type CanvasBatch, type Point } from './protocol.ts'

/**
 * Paints a list of batches and remembers how far it got. The room state only ever appends
 * batches or removes them (undo, a new round), so when the batch it drew last is still in
 * place it draws just the new ones; anything else repaints from scratch.
 */
export class CanvasPainter {
  private drawn = 0
  private lastDrawn: CanvasBatch | null = null
  /** The last point drawn for each stroke, so the next batch continues the line. */
  private tails = new Map<string, Point>()

  /** Forget what was drawn (after a resize, which clears the canvas). */
  reset() {
    this.drawn = 0
    this.lastDrawn = null
    this.tails.clear()
  }

  paint(ctx: CanvasRenderingContext2D, batches: readonly CanvasBatch[], width: number, height: number) {
    const appendOnly =
      batches.length >= this.drawn && (this.drawn === 0 || batches[this.drawn - 1] === this.lastDrawn)
    if (!appendOnly || this.drawn === 0) {
      ctx.clearRect(0, 0, width, height)
      this.reset()
    }
    for (let i = this.drawn; i < batches.length; i++) {
      const b = batches[i]
      const key = `${b.user_id}/${b.stroke_id}`
      drawPoints(ctx, b, this.tails.get(key) ?? null, width, height)
      this.tails.set(key, b.points[b.points.length - 1])
    }
    this.drawn = batches.length
    this.lastDrawn = batches.length > 0 ? batches[batches.length - 1] : null
  }
}

/** Draws one batch, continuing from prev (the stroke's last point) when there is one. */
export function drawPoints(
  ctx: CanvasRenderingContext2D,
  b: Pick<CanvasBatch, 'points' | 'color' | 'width'>,
  prev: Point | null,
  width: number,
  height: number,
) {
  if (b.points.length === 0) return
  const sx = width / MAX_COORD
  const sy = height / MAX_COORD
  const lineWidth = Math.max(1, b.width * sx)
  ctx.strokeStyle = b.color
  ctx.fillStyle = b.color
  ctx.lineWidth = lineWidth
  ctx.lineCap = 'round'
  ctx.lineJoin = 'round'

  const pts = prev ? [prev, ...b.points] : b.points
  if (pts.length === 1) {
    // A tap: draw a dot.
    ctx.beginPath()
    ctx.arc(pts[0][0] * sx, pts[0][1] * sy, lineWidth / 2, 0, Math.PI * 2)
    ctx.fill()
    return
  }
  ctx.beginPath()
  ctx.moveTo(pts[0][0] * sx, pts[0][1] * sy)
  for (let i = 1; i < pts.length; i++) ctx.lineTo(pts[i][0] * sx, pts[i][1] * sy)
  ctx.stroke()
}
