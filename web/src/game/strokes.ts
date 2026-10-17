// Turns pointer input into stroke_batch messages (design doc, section 6.4). Points pile up and
// go out every batch interval, or at once when a batch reaches 512 points, so the server gets
// a few messages per second per drawer instead of one per pointer event.

import {
  MAX_COORD,
  MAX_POINTS_PER_BATCH,
  MAX_STROKE_TIME_MS,
  type Point,
  type StrokeBatch,
} from './protocol.ts'

/** Maps a position inside an element to normalized 0..10000 coordinates. */
export function normalize(x: number, y: number, width: number, height: number): [number, number] {
  const clamp = (v: number) => Math.min(MAX_COORD, Math.max(0, Math.round(v)))
  return [clamp((x / width) * MAX_COORD), clamp((y / height) * MAX_COORD)]
}

/** A stroke id the server accepts ([A-Za-z0-9_-]{1,32}): 32 random hex digits. */
export function newStrokeId(): string {
  return crypto.randomUUID().replaceAll('-', '')
}

type Options = {
  intervalMs: number
  now?: () => number
  newId?: () => string
}

export class StrokeBatcher {
  private readonly send: (batch: StrokeBatch) => void
  private readonly intervalMs: number
  private readonly now: () => number
  private readonly newId: () => string

  private stroke: { id: string; color: string; width: number; startedAt: number } | null = null
  private pending: Point[] = []
  private last: Point | null = null
  private timer: ReturnType<typeof setInterval> | null = null

  constructor(send: (batch: StrokeBatch) => void, opts: Options) {
    this.send = send
    this.intervalMs = opts.intervalMs
    this.now = opts.now ?? (() => performance.now())
    this.newId = opts.newId ?? newStrokeId
  }

  /** Starts a stroke at a normalized point and returns its id. */
  begin(x: number, y: number, color: string, width: number): string {
    this.cancel()
    this.stroke = { id: this.newId(), color, width, startedAt: this.now() }
    this.add(x, y)
    this.timer = setInterval(() => this.flush(false), this.intervalMs)
    return this.stroke.id
  }

  /** Adds a normalized point to the current stroke. A repeat of the last point is dropped. */
  add(x: number, y: number) {
    if (!this.stroke) return
    if (this.last && this.last[0] === x && this.last[1] === y) return
    const t = Math.min(MAX_STROKE_TIME_MS, Math.round(this.now() - this.stroke.startedAt))
    const p: Point = [x, y, t]
    this.pending.push(p)
    this.last = p
    if (this.pending.length >= MAX_POINTS_PER_BATCH) this.flush(false)
  }

  /** Ends the stroke: sends what is left with done = true. */
  end() {
    if (!this.stroke) return
    if (this.pending.length === 0 && this.last) this.pending.push(this.last) // done needs a point
    this.flush(true)
    this.stop()
  }

  /** Drops the current stroke without sending the rest (the round ended, the socket closed). */
  cancel() {
    this.stop()
  }

  get active(): boolean {
    return this.stroke !== null
  }

  private flush(done: boolean) {
    if (!this.stroke || this.pending.length === 0) return
    const { id, color, width } = this.stroke
    const points = this.pending
    this.pending = []
    this.send({ stroke_id: id, points, color, width, done })
  }

  private stop() {
    if (this.timer) clearInterval(this.timer)
    this.timer = null
    this.stroke = null
    this.pending = []
    this.last = null
  }
}
