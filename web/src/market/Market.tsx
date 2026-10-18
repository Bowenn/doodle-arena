// The marketplace: every active listing, newest first.

import { useEffect, useState } from 'react'
import { ApiError } from '../api.ts'
import { playerLabel } from '../gallery/names.ts'
import { coins, listListings, type Listing, type Sale } from '../money.ts'
import { BuyButton } from './BuyButton.tsx'
import { DrawingThumb } from './DrawingThumb.tsx'
import { useDrawings } from './useDrawings.ts'

type Props = {
  meId: string
  onOpen: (drawingId: string) => void
  onWallet: () => void
}

export function Market({ meId, onOpen, onWallet }: Props) {
  const [listings, setListings] = useState<Listing[] | null>(null)
  const [cursor, setCursor] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [bought, setBought] = useState<Sale | null>(null)
  const drawings = useDrawings(listings?.map((l) => l.drawing_id) ?? [])

  useEffect(() => {
    listListings()
      .then((page) => {
        setListings(page.listings)
        setCursor(page.next_cursor)
      })
      .catch((e: unknown) => setError(e instanceof ApiError ? e.message : 'Could not load the market.'))
  }, [])

  function loadMore() {
    listListings({ cursor })
      .then((page) => {
        setListings((prev) => [...(prev ?? []), ...page.listings])
        setCursor(page.next_cursor)
      })
      .catch((e: unknown) => setError(e instanceof ApiError ? e.message : 'Could not load more listings.'))
  }

  const drop = (id: string) => setListings((prev) => prev?.filter((l) => l.listing_id !== id) ?? null)

  return (
    <main>
      <h1>Market</h1>
      <p className="muted">
        Buy other players' drawings with coins. When a drawing is resold, its other authors share a 10% royalty.
      </p>
      {bought && (
        <p role="status" className="notice good">
          You bought “{drawings.get(bought.drawing_id)?.word ?? 'a drawing'}” for {coins(bought.price)}. It's in My
          drawings.
        </p>
      )}
      {error && <p role="alert">{error}</p>}
      {listings === null ? (
        !error && <p>Loading…</p>
      ) : listings.length === 0 ? (
        <p>Nothing is for sale right now. List one of your drawings from My drawings.</p>
      ) : (
        <ul className="gallery-grid">
          {listings.map((l) => (
            <li key={l.listing_id}>
              <DrawingThumb drawingId={l.drawing_id} drawing={drawings.get(l.drawing_id)} onOpen={onOpen} />
              <span className="price">{coins(l.price)}</span>
              <span className="muted">Sold by {l.seller_id === meId ? 'you' : playerLabel(l.seller_id)}</span>
              {l.seller_id !== meId && (
                <BuyButton
                  listing={l}
                  onBought={(sale) => {
                    setBought(sale)
                    drop(l.listing_id)
                  }}
                  onGone={() => {}}
                  onWallet={onWallet}
                />
              )}
            </li>
          ))}
        </ul>
      )}
      {cursor && (
        <button type="button" onClick={loadMore}>
          Load more
        </button>
      )}
    </main>
  )
}
