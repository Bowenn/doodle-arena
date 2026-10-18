// Plays a stored replay back the way it was drawn (task 3.5). The game server records, for
// every stroke, when it received the stroke's last batch (server_received_ms, after the round
// started) and each point's offset from the stroke's first point (t_ms, client clock). So a
// stroke started at server_received_ms minus its last point's t_ms, and each point lands at
// that start plus its t_ms. The replay starts with the first point, and long pauses (a drawer
// thinking) are squeezed so it keeps moving. Undone strokes are left out: the log says that a stroke was undone, not when, and
// the replay then ends on the same picture as the PNG.

import type { Replay, ReplayPoint } from '../gallery.ts'
import { drawPoints } from './render.ts'

/** Point `point` of visible stroke `stroke` appears `at` ms into the replay. */
export type ReplayEvent = { at: number; stroke: number; point: number }

export type ReplayStrokeView = { color: string; width: number; points: ReplayPoint[] }

export type Timeline = { strokes: ReplayStrokeView[]; events: ReplayEvent[]; duration: number }

export const MAX_GAP_MS = 1000

export function timeline(replay: Replay, maxGapMs = MAX_GAP_MS): Timeline {
  const strokes = replay.strokes.filter((s) => !s.undone && s.points.length > 0)
  const raw: ReplayEvent[] = []
  strokes.forEach((s, i) => {
    const last = s.points[s.points.length - 1][2]
    const start = Math.max(0, s.server_received_ms - last)
    s.points.forEach((p, j) => raw.push({ at: start + Math.max(0, p[2]), stroke: i, point: j }))
  })
  // Stable sort keeps each stroke's own points in order, and strokes in server order on ties.
  raw.sort((a, b) => a.at - b.at)

  // Start at the first point, and squeeze every later gap longer than maxGapMs.
  const events: ReplayEvent[] = []
  let prevRaw = raw.length > 0 ? raw[0].at : 0
  let clock = 0
  for (const e of raw) {
    clock += Math.min(e.at - prevRaw, maxGapMs)
    prevRaw = e.at
    events.push({ ...e, at: clock })
  }
  return {
    strokes: strokes.map((s) => ({ color: s.color, width: s.width, points: s.points })),
    events,
    duration: events.length > 0 ? events[events.length - 1].at : 0,
  }
}

/** How many events have happened by `ms` into the replay (events are sorted by time). */
export function eventsUntil(events: readonly ReplayEvent[], ms: number): number {
  let lo = 0
  let hi = events.length
  while (lo < hi) {
    const mid = (lo + hi) >> 1
    if (events[mid].at <= ms) lo = mid + 1
    else hi = mid
  }
  return lo
}

/**
 * Draws a timeline up to a number of events, with the live canvas's drawPoints, so a replay
 * looks exactly like the round did. Moving forward only draws the new segments; moving back
 * (restart) repaints from scratch.
 */
export class ReplayPainter {
  private drawn = 0
  private tails = new Map<number, ReplayPoint>()

  paint(ctx: CanvasRenderingContext2D, tl: Timeline, upTo: number, width: number, height: number) {
    if (upTo < this.drawn) {
      ctx.clearRect(0, 0, width, height)
      this.drawn = 0
      this.tails.clear()
    }
    for (let i = this.drawn; i < upTo; i++) {
      const e = tl.events[i]
      const s = tl.strokes[e.stroke]
      const p = s.points[e.point]
      drawPoints(ctx, { color: s.color, width: s.width, points: [p] }, this.tails.get(e.stroke) ?? null, width, height)
      this.tails.set(e.stroke, p)
    }
    this.drawn = Math.max(this.drawn, upTo)
  }
}
