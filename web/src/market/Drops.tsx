// Limited drops (ADR 0022): N numbered editions of a drawing, sold from a set time. Buyers join
// the waiting room before the open (places are drawn by lottery when it opens), then buy one
// edition each once admitted. Admins create drops here too.

import { useEffect, useState, type FormEvent } from 'react'
import { ApiError } from '../api.ts'
import { useIdempotencyKeys } from '../idempotency.ts'
import {
  buyEdition,
  coins,
  createDrop,
  dropQueue,
  joinDrop,
  listDrops,
  type Drop,
  type Edition,
  type QueueStatus,
} from '../money.ts'
import { countdown } from './countdown.ts'
import { DrawingThumb } from './DrawingThumb.tsx'
import { useDrawings } from './useDrawings.ts'

/** How often a card asks for its place while the drop is upcoming or open. */
export const QUEUE_POLL_MS = 2000

type Props = { isAdmin: boolean; onOpen: (drawingId: string) => void; onWallet: () => void }

export function Drops({ isAdmin, onOpen, onWallet }: Props) {
  const [drops, setDrops] = useState<Drop[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const drawings = useDrawings(drops?.map((d) => d.drawing_id) ?? [])

  useEffect(() => {
    listDrops()
      .then(setDrops)
      .catch((e: unknown) => setError(e instanceof ApiError ? e.message : 'Could not load the drops.'))
  }, [])

  return (
    <main>
      <h1>Drops</h1>
      <p className="muted">
        Limited editions of a drawing, released at a set time. Join the waiting room before it opens: places are
        drawn by lottery at the open, so there is no need to rush. One edition per player.
      </p>
      {isAdmin && <CreateDrop onCreated={(d) => setDrops((prev) => [d, ...(prev ?? [])])} />}
      {error && <p role="alert">{error}</p>}
      {drops === null ? (
        !error && <p>Loading…</p>
      ) : drops.length === 0 ? (
        <p>No drops right now. Check back later.</p>
      ) : (
        <ul className="gallery-grid">
          {drops.map((d) => (
            <li key={d.drop_id} className="market-card">
              <DrawingThumb drawingId={d.drawing_id} drawing={drawings.get(d.drawing_id)} onOpen={onOpen} />
              <DropCard drop={d} onWallet={onWallet} />
            </li>
          ))}
        </ul>
      )}
    </main>
  )
}

/** The current time, ticking every second while mounted. */
function useNow(): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [])
  return now
}

type Outcome = { tone: 'good' | 'bad'; text: string; wallet?: boolean }

function DropCard({ drop, onWallet }: { drop: Drop; onWallet: () => void }) {
  const now = useNow()
  const [queue, setQueue] = useState<QueueStatus | null>(null)
  const [bought, setBought] = useState<Edition | null>(null)
  const [busy, setBusy] = useState(false)
  const [outcome, setOutcome] = useState<Outcome | null>(null)
  const keys = useIdempotencyKeys()

  const opens = Date.parse(drop.opens_at)
  const closes = Date.parse(drop.closes_at)
  const phase = now < opens ? 'UPCOMING' : now < closes ? 'OPEN' : 'CLOSED'
  const left = queue?.editions_left ?? drop.editions_left
  const soldOut = left <= 0 || queue?.status === 'SOLD_OUT'
  const joined = queue?.position !== null && queue?.position !== undefined

  // Once joined, ask for the place every few seconds until the drop is over or bought.
  useEffect(() => {
    if (!joined || bought || phase === 'CLOSED') return
    const id = setInterval(() => {
      dropQueue(drop.drop_id)
        .then(setQueue)
        .catch(() => {
          // A missed poll is fine: the next one tries again.
        })
    }, QUEUE_POLL_MS)
    return () => clearInterval(id)
  }, [joined, bought, phase, drop.drop_id])

  function join() {
    setBusy(true)
    setOutcome(null)
    joinDrop(drop.drop_id)
      .then(setQueue)
      .catch((e: unknown) => setOutcome({ tone: 'bad', text: explain(e) }))
      .finally(() => setBusy(false))
  }

  function buy() {
    setBusy(true)
    setOutcome(null)
    keys
      .run(`drop:${drop.drop_id}`, (key) => buyEdition(drop.drop_id, key))
      .then((edition) => {
        setBought(edition)
        setOutcome({ tone: 'good', text: `You got edition #${edition.edition_no} of ${edition.editions}.` })
      })
      .catch((e: unknown) => {
        const code = e instanceof ApiError ? e.code : ''
        setOutcome({ tone: 'bad', text: explain(e), wallet: code === 'INSUFFICIENT_FUNDS' })
      })
      .finally(() => setBusy(false))
  }

  let status
  if (phase === 'UPCOMING') status = `Opens in ${countdown(opens - now)}`
  else if (phase === 'CLOSED') status = 'Closed'
  else if (soldOut && !bought) status = 'Sold out'
  else status = `Open: closes in ${countdown(closes - now)}`

  return (
    <div className="drop">
      <p>
        <strong>{coins(drop.price)}</strong> · {left} of {drop.editions} left
      </p>
      <p className="muted" role="timer">
        {status}
      </p>
      {bought ? null : !joined ? (
        <button type="button" disabled={busy || phase === 'CLOSED' || soldOut} onClick={join}>
          Join the waiting room
        </button>
      ) : phase === 'UPCOMING' ? (
        <p>You're in. Places are drawn when the drop opens.</p>
      ) : queue?.admitted ? (
        <button type="button" className="primary" disabled={busy || soldOut || phase !== 'OPEN'} onClick={buy}>
          Buy for {coins(drop.price)}
        </button>
      ) : (
        <p>
          You're in line: {queue?.position} {queue?.position === 1 ? 'place' : 'places'} ahead of you.
        </p>
      )}
      {outcome && (
        <p className={`notice ${outcome.tone}`} role="status">
          {outcome.text}{' '}
          {outcome.wallet && (
            <a
              href="/wallet"
              onClick={(e) => {
                e.preventDefault()
                onWallet()
              }}
            >
              Get coins
            </a>
          )}
        </p>
      )}
    </div>
  )
}

/** A refusal, in words. */
function explain(e: unknown): string {
  if (!(e instanceof ApiError)) return 'Could not reach the server. Try again.'
  switch (e.code) {
    case 'SOLD_OUT':
      return 'Every edition is taken.'
    case 'ALREADY_BOUGHT':
      return 'You already have an edition of this drop.'
    case 'NOT_ADMITTED':
      return "It isn't your turn yet: stay in the waiting room."
    case 'INSUFFICIENT_FUNDS':
      return "You don't have enough coins."
    case 'DROP_NOT_OPEN':
      return "The drop hasn't opened yet."
    case 'DROP_CLOSED':
      return 'The drop has closed.'
    case 'RATE_LIMITED':
      return 'Too many requests: wait a moment.'
    case 'DROPS_UNAVAILABLE':
    case 'DROP_CONFLICT':
    case 'IDEMPOTENCY_KEY_IN_PROGRESS':
      return 'That didn’t go through: try again.'
    default:
      return e.message
  }
}

/** Admins only: release a drop of an approved drawing. */
function CreateDrop({ onCreated }: { onCreated: (d: Drop) => void }) {
  const [drawingId, setDrawingId] = useState('')
  const [editions, setEditions] = useState('10')
  const [price, setPrice] = useState('25')
  const [opensIn, setOpensIn] = useState('5')
  const [duration, setDuration] = useState('60')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const keys = useIdempotencyKeys()

  function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    const drop = {
      drawing_id: drawingId.trim(),
      editions: Number(editions),
      price: Number(price),
      opens_at: new Date(Date.now() + Number(opensIn) * 60_000).toISOString(),
      duration_minutes: Number(duration),
    }
    keys
      .run('create-drop', (key) => createDrop(drop, key))
      .then((d) => {
        onCreated(d)
        setDrawingId('')
      })
      .catch((err: unknown) => setError(err instanceof ApiError ? err.message : 'Could not reach the server.'))
      .finally(() => setBusy(false))
  }

  return (
    <form className="create-drop" onSubmit={submit} aria-label="Create a drop">
      <h2>New drop</h2>
      <label>
        Drawing id <input value={drawingId} onChange={(e) => setDrawingId(e.target.value)} required />
      </label>
      <label>
        Editions <input type="number" min={1} max={1000} value={editions} onChange={(e) => setEditions(e.target.value)} />
      </label>
      <label>
        Price <input type="number" min={1} max={100000} value={price} onChange={(e) => setPrice(e.target.value)} />
      </label>
      <label>
        Opens in (minutes) <input type="number" min={1} value={opensIn} onChange={(e) => setOpensIn(e.target.value)} />
      </label>
      <label>
        Open for (minutes){' '}
        <input type="number" min={1} max={1440} value={duration} onChange={(e) => setDuration(e.target.value)} />
      </label>
      <button type="submit" disabled={busy || !drawingId.trim()}>
        Create the drop
      </button>
      {error && <p role="alert">{error}</p>}
    </form>
  )
}
