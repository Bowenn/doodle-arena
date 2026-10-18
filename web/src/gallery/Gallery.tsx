// The gallery: everyone's approved drawings, your own (in any state), or one match's.

import { useCallback, useEffect, useState } from 'react'
import { ApiError } from '../api.ts'
import { listDrawings, type Drawing, type DrawingQuery } from '../gallery.ts'
import { STATUS_LABEL } from './names.ts'

type Tab = 'all' | 'mine'
type Props = {
  meId: string
  /** Set when coming from the end of a match: that match's drawings only. */
  matchId?: string | null
  onOpen: (drawingId: string) => void
  onBack: () => void
}

export function Gallery({ meId, matchId = null, onOpen, onBack }: Props) {
  const [tab, setTab] = useState<Tab>('all')
  const query: DrawingQuery = matchId ? { matchId } : tab === 'mine' ? { authorId: meId } : {}

  return (
    <main>
      <h1>{matchId ? "This match's drawings" : 'Drawing gallery'}</h1>
      <div className="actions">
        <button type="button" onClick={onBack}>
          Back to lobby
        </button>
        {!matchId &&
          (['all', 'mine'] as const).map((t) => (
            <button key={t} type="button" aria-pressed={tab === t} onClick={() => setTab(t)}>
              {t === 'all' ? 'All drawings' : 'My drawings'}
            </button>
          ))}
      </div>
      {matchId && <p className="muted">Each round's drawing appears here a few seconds after the round, once it's moderated.</p>}
      {/* Keyed by the query, so switching tabs starts a fresh list. */}
      <DrawingGrid
        key={JSON.stringify(query)}
        query={query}
        inRoundOrder={Boolean(matchId)}
        refreshable={Boolean(matchId)}
        emptyText={tab === 'mine' && !matchId ? "You haven't drawn anything yet. Play a round!" : 'No drawings yet.'}
        onOpen={onOpen}
      />
    </main>
  )
}

type GridProps = {
  query: DrawingQuery
  inRoundOrder: boolean
  refreshable: boolean
  emptyText: string
  onOpen: (drawingId: string) => void
}

function DrawingGrid({ query, inRoundOrder, refreshable, emptyText, onOpen }: GridProps) {
  const [drawings, setDrawings] = useState<Drawing[] | null>(null)
  const [cursor, setCursor] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [query0] = useState(query) // the key changes with the query, so it's fixed here

  const load = useCallback(
    (from: string | null) => {
      setLoading(true)
      listDrawings({ ...query0, cursor: from })
        .then((page) => {
          setDrawings((prev) => (from ? [...(prev ?? []), ...page.drawings] : page.drawings))
          setCursor(page.next_cursor)
          setError(null)
        })
        .catch((e: unknown) => setError(e instanceof ApiError ? e.message : 'Could not load the drawings.'))
        .finally(() => setLoading(false))
    },
    [query0],
  )

  useEffect(() => {
    listDrawings({ ...query0, cursor: null })
      .then((page) => {
        setDrawings(page.drawings)
        setCursor(page.next_cursor)
      })
      .catch((e: unknown) => setError(e instanceof ApiError ? e.message : 'Could not load the drawings.'))
  }, [query0])

  // A match's drawings: in round order. The rest: newest first, as the server sends them.
  const shown = inRoundOrder && drawings ? [...drawings].sort((a, b) => a.round_no - b.round_no) : drawings

  return (
    <>
      {refreshable && (
        <div className="actions">
          <button type="button" disabled={loading} onClick={() => load(null)}>
            Refresh
          </button>
        </div>
      )}
      {error && <p role="alert">{error}</p>}
      {shown === null ? (
        !error && <p>Loading…</p>
      ) : shown.length === 0 ? (
        <p>{emptyText}</p>
      ) : (
        <ul className="gallery-grid">
          {shown.map((d) => (
            <li key={d.id}>
              <a
                href={`/drawings/${d.id}`}
                className="drawing-card"
                onClick={(e) => {
                  e.preventDefault()
                  onOpen(d.id)
                }}
              >
                {d.thumb_url ? (
                  <img src={d.thumb_url} alt={`A drawing of "${d.word}"`} width={256} height={192} loading="lazy" />
                ) : (
                  <span className="thumb-placeholder">No image yet</span>
                )}
                <span className="drawing-word">{d.word}</span>
                {d.status !== 'approved' && <span className="badge">{STATUS_LABEL[d.status] ?? d.status}</span>}
              </a>
            </li>
          ))}
        </ul>
      )}
      {cursor && (
        <button type="button" disabled={loading} onClick={() => load(cursor)}>
          Load more
        </button>
      )}
    </>
  )
}
