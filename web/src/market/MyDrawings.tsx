// My drawings: the drawings I own (bought, or drawn and minted to me), to list for sale or take back,
// and the numbered editions I bought in limited drops.

import { useEffect, useState } from 'react'
import { ApiError } from '../api.ts'
import { useIdempotencyKeys } from '../idempotency.ts'
import {
  cancelListing,
  coins,
  createListing,
  listEditions,
  listOwnedDrawings,
  MAX_PRICE,
  type Edition,
  type OwnedDrawing,
} from '../money.ts'
import { DrawingThumb } from './DrawingThumb.tsx'
import { useDrawings } from './useDrawings.ts'

type Props = { onOpen: (drawingId: string) => void }

export function MyDrawings({ onOpen }: Props) {
  const [owned, setOwned] = useState<OwnedDrawing[] | null>(null)
  const [cursor, setCursor] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const drawings = useDrawings(owned?.map((o) => o.drawing_id) ?? [])

  useEffect(() => {
    listOwnedDrawings()
      .then((page) => {
        setOwned(page.drawings)
        setCursor(page.next_cursor)
      })
      .catch((e: unknown) => setError(e instanceof ApiError ? e.message : 'Could not load your drawings.'))
  }, [])

  function loadMore() {
    listOwnedDrawings(cursor)
      .then((page) => {
        setOwned((prev) => [...(prev ?? []), ...page.drawings])
        setCursor(page.next_cursor)
      })
      .catch((e: unknown) => setError(e instanceof ApiError ? e.message : 'Could not load more drawings.'))
  }

  function update(next: OwnedDrawing) {
    setOwned((prev) => prev?.map((o) => (o.drawing_id === next.drawing_id ? next : o)) ?? null)
  }

  return (
    <main>
      <h1>My drawings</h1>
      <p className="muted">Drawings you own: the ones minted to you after a round, and the ones you bought.</p>
      {error && <p role="alert">{error}</p>}
      {owned === null ? (
        !error && <p>Loading…</p>
      ) : owned.length === 0 ? (
        <p>You don't own any drawings yet. Draw in a match, or buy one in the Market.</p>
      ) : (
        <ul className="gallery-grid">
          {owned.map((o) => (
            <li key={o.drawing_id} className="market-card">
              <DrawingThumb drawingId={o.drawing_id} drawing={drawings.get(o.drawing_id)} onOpen={onOpen} />
              <SellControls owned={o} onChange={update} />
            </li>
          ))}
        </ul>
      )}
      {cursor && (
        <button type="button" onClick={loadMore}>
          Load more
        </button>
      )}
      <Editions onOpen={onOpen} />
    </main>
  )
}

function SellControls({ owned, onChange }: { owned: OwnedDrawing; onChange: (o: OwnedDrawing) => void }) {
  const [price, setPrice] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const keys = useIdempotencyKeys()

  const failed = (e: unknown) => {
    setBusy(false)
    setError(e instanceof ApiError ? e.message : 'Could not reach the server. Try again.')
  }

  function list() {
    const n = Number(price)
    if (!/^\d+$/.test(price.trim()) || n < 1 || n > MAX_PRICE) {
      setError(`Enter a whole number of coins from 1 to ${MAX_PRICE.toLocaleString('en-US')}.`)
      return
    }
    setBusy(true)
    setError(null)
    keys
      .run(`list:${owned.drawing_id}:${n}`, (key) => createListing(owned.drawing_id, n, key))
      .then((l) => {
        setBusy(false)
        setPrice('')
        onChange({ ...owned, listing: { listing_id: l.listing_id, price: l.price, created_at: l.created_at } })
      })
      .catch(failed)
  }

  function cancel(listingId: string) {
    setBusy(true)
    setError(null)
    cancelListing(listingId)
      .then(() => {
        setBusy(false)
        onChange({ ...owned, listing: null })
      })
      .catch(failed)
  }

  const listing = owned.listing
  return (
    <>
      {listing ? (
        <span className="actions">
          <span className="price">Listed for {coins(listing.price)}</span>
          <button type="button" disabled={busy} onClick={() => cancel(listing.listing_id)}>
            Cancel listing
          </button>
        </span>
      ) : (
        <form
          className="actions"
          onSubmit={(e) => {
            e.preventDefault()
            list()
          }}
        >
          <input
            aria-label="Price in coins"
            inputMode="numeric"
            placeholder="Price"
            size={8}
            value={price}
            onChange={(e) => setPrice(e.target.value)}
          />
          <button type="submit" disabled={busy}>
            List for sale
          </button>
        </form>
      )}
      {error && (
        <span role="alert" className="notice bad">
          {error}
        </span>
      )}
    </>
  )
}

/** Editions bought in limited drops (ADR 0022). They can't be resold. */
function Editions({ onOpen }: Props) {
  const [editions, setEditions] = useState<Edition[] | null>(null)
  const drawings = useDrawings(editions?.map((e) => e.drawing_id) ?? [])

  useEffect(() => {
    listEditions()
      .then(setEditions)
      .catch(() => setEditions([]))
  }, [])

  if (!editions || editions.length === 0) return null
  return (
    <section aria-label="Editions">
      <h2>Editions</h2>
      <p className="muted">Numbered editions you bought in drops.</p>
      <ul className="gallery-grid">
        {editions.map((e) => (
          <li key={`${e.drop_id}:${e.edition_no}`} className="market-card">
            <DrawingThumb drawingId={e.drawing_id} drawing={drawings.get(e.drawing_id)} onOpen={onOpen} />
            <p>
              Edition #{e.edition_no} of {e.editions} · {coins(e.price)}
            </p>
          </li>
        ))}
      </ul>
    </section>
  )
}
