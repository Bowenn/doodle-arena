// Plays a drawing's replay on a canvas, at its recorded pace (game/replay.ts).

import { useEffect, useMemo, useRef, useState } from 'react'
import type { Replay } from '../gallery.ts'
import { eventsUntil, ReplayPainter, timeline } from '../game/replay.ts'

const WIDTH = 640
const TICK_MS = 33
const SPEEDS = [1, 2, 4] as const

export function ReplayPlayer({ replay }: { replay: Replay }) {
  const tl = useMemo(() => timeline(replay), [replay])
  const height = Math.round(
    replay.canvas_width > 0 && replay.canvas_height > 0
      ? (WIDTH * replay.canvas_height) / replay.canvas_width
      : (WIDTH * 3) / 4,
  )
  const canvas = useRef<HTMLCanvasElement | null>(null)
  const painter = useRef(new ReplayPainter())
  // The timer advances the ref; the state copy re-renders the canvas and the clock.
  const pos = useRef(0)
  const [position, setPosition] = useState(0)
  const [playing, setPlaying] = useState(true)
  const [speed, setSpeed] = useState<(typeof SPEEDS)[number]>(1)

  useEffect(() => {
    if (!playing) return
    let last = Date.now()
    const t = setInterval(() => {
      const now = Date.now()
      pos.current = Math.min(tl.duration, pos.current + (now - last) * speed)
      last = now
      setPosition(pos.current)
      if (pos.current >= tl.duration) setPlaying(false)
    }, TICK_MS)
    return () => clearInterval(t)
  }, [playing, speed, tl.duration])

  useEffect(() => {
    const ctx = canvas.current?.getContext('2d')
    if (ctx) painter.current.paint(ctx, tl, eventsUntil(tl.events, position), WIDTH, height)
  }, [position, tl, height])

  function seek(ms: number) {
    pos.current = ms
    setPosition(ms)
  }

  function playPause() {
    if (!playing && pos.current >= tl.duration) seek(0) // play again from the start
    setPlaying(!playing)
  }

  return (
    <section className="replay" aria-label="Replay">
      <canvas ref={canvas} width={WIDTH} height={height} className="replay-canvas" />
      <div className="actions">
        <button type="button" className="primary" onClick={playPause}>
          {playing ? 'Pause' : 'Play'}
        </button>
        <button
          type="button"
          onClick={() => {
            seek(0)
            setPlaying(true)
          }}
        >
          Restart
        </button>
        {SPEEDS.map((s) => (
          <button key={s} type="button" aria-pressed={speed === s} onClick={() => setSpeed(s)}>
            {s}×
          </button>
        ))}
        <span className="muted" aria-label="Replay position">
          {(position / 1000).toFixed(1)} s / {(tl.duration / 1000).toFixed(1)} s
        </span>
      </div>
    </section>
  )
}
