// A drawing's owner, its provenance (mint, then each sale with its price), and Buy when it's for sale.

import { useEffect, useState } from 'react'
import { ApiError } from '../api.ts'
import { playerLabel } from '../gallery/names.ts'
import { coins, getOwnershipHistory, listListings, type Listing, type OwnershipHistory } from '../money.ts'
import { BuyButton } from './BuyButton.tsx'

type Props = { drawingId: string; meId: string; onWallet: () => void }

export function Ownership({ drawingId, meId, onWallet }: Props) {
  const [history, setHistory] = useState<OwnershipHistory | null>(null)
  const [listing, setListing] = useState<Listing | null>(null)
  const [state, setState] = useState<'loading' | 'ready' | 'not-minted' | 'error'>('loading')
  const [version, setVersion] = useState(0) // bumped to reload after a purchase

  useEffect(() => {
    let cancelled = false
    Promise.all([getOwnershipHistory(drawingId), listListings({ drawingId })])
      .then(([h, page]) => {
        if (cancelled) return
        setHistory(h)
        setListing(page.listings[0] ?? null)
        setState('ready')
      })
      .catch((e: unknown) => {
        if (cancelled) return
        // Approved drawings are minted within seconds; until then there's no owner yet.
        setState(e instanceof ApiError && e.status === 404 ? 'not-minted' : 'error')
      })
    return () => {
      cancelled = true
    }
  }, [drawingId, version])

  const who = (id: string | null) => (id === null ? 'nobody' : id === meId ? 'you' : playerLabel(id))

  if (state === 'loading') return null
  if (state === 'not-minted') return <p className="muted">This drawing isn't tradable yet.</p>
  if (state === 'error' || !history) return <p role="alert">Could not load who owns this drawing.</p>
  return (
    <section>
      <h2>Ownership</h2>
      <p>
        Owned by <strong>{who(history.owner_id)}</strong>
        {listing && (
          <>
            {' '}
            · for sale for <span className="price">{coins(listing.price)}</span>
          </>
        )}
      </p>
      {listing && listing.seller_id !== meId && (
        <BuyButton
          listing={listing}
          onBought={() => setVersion((v) => v + 1)}
          onGone={() => setVersion((v) => v + 1)}
          onWallet={onWallet}
        />
      )}
      <ol className="plain-list">
        {history.events.map((e, i) => (
          <li key={i}>
            <span className="muted">{new Date(e.created_at).toLocaleString()}</span>
            {e.kind === 'MINT' ? (
              <span>Minted to {who(e.to_owner)}</span>
            ) : (
              <span>
                Sold by {who(e.from_owner)} to {who(e.to_owner)}
                {e.price !== null && <> for {coins(e.price)}</>}
              </span>
            )}
          </li>
        ))}
      </ol>
    </section>
  )
}
