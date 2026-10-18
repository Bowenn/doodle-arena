// One drawing: the image, who drew it, its replay, and who owns it.

import { useEffect, useState } from 'react'
import { ApiError } from '../api.ts'
import { getDrawing, getReplay, type Drawing, type Replay } from '../gallery.ts'
import { Ownership } from '../market/Ownership.tsx'
import { playerLabel, STATUS_LABEL } from './names.ts'
import { ReplayPlayer } from './ReplayPlayer.tsx'

type Props = { drawingId: string; meId: string; onBack: () => void; onWallet?: () => void }

export function DrawingPage({ drawingId, meId, onBack, onWallet = () => {} }: Props) {
  const [drawing, setDrawing] = useState<Drawing | null>(null)
  const [replay, setReplay] = useState<Replay | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    getDrawing(drawingId)
      .then(async (d) => {
        if (cancelled) return
        setDrawing(d)
        if (d.has_replay) {
          const r = await getReplay(drawingId)
          if (!cancelled) setReplay(r)
        }
      })
      .catch((e: unknown) => {
        if (cancelled) return
        if (e instanceof ApiError && e.status === 404) setError("This drawing isn't available.")
        else setError('Could not load this drawing.')
      })
    return () => {
      cancelled = true
    }
  }, [drawingId])

  return (
    <main>
      <div className="actions">
        <button type="button" onClick={onBack}>
          Back to the gallery
        </button>
      </div>
      {error && <p role="alert">{error}</p>}
      {!drawing && !error && <p>Loading…</p>}
      {drawing && (
        <>
          <h1>“{drawing.word}”</h1>
          <p className="muted">
            Round {drawing.round_no}, drawn by{' '}
            {drawing.author_ids.map((id) => (id === meId ? 'you' : playerLabel(id))).join(' and ')}
          </p>
          {drawing.status !== 'approved' && <p className="badge">{STATUS_LABEL[drawing.status] ?? drawing.status}</p>}
          {replay ? (
            <ReplayPlayer replay={replay} />
          ) : (
            drawing.image_url && <img className="drawing-full" src={drawing.image_url} alt={`A drawing of "${drawing.word}"`} />
          )}
          {drawing.status === 'approved' && <Ownership drawingId={drawingId} meId={meId} onWallet={onWallet} />}
        </>
      )}
    </main>
  )
}
