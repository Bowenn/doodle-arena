// The marketplace: every active listing, newest first, or a search of them (ADR 0023): words,
// a price range with its counts, and a sort.

import { useEffect, useState, type FormEvent } from 'react'
import { ApiError } from '../api.ts'
import type { PriceFacet, SearchQuery, SearchSort } from '../gallery.ts'
import { playerLabel } from '../gallery/names.ts'
import { SearchResults } from '../gallery/SearchResults.tsx'
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

  const [text, setText] = useState('')
  const [min, setMin] = useState('')
  const [max, setMax] = useState('')
  const [sort, setSort] = useState<SearchSort>('relevance')
  const [search, setSearch] = useState<SearchQuery | null>(null)
  const [facets, setFacets] = useState<PriceFacet[]>([])

  function submit(e: FormEvent) {
    e.preventDefault()
    const whole = (v: string) => (/^[0-9]+$/.test(v) && Number(v) > 0 ? Number(v) : undefined)
    setSearch({ q: text.trim(), listed: true, minPrice: whole(min), maxPrice: whole(max), sort })
  }

  function byFacet(f: PriceFacet) {
    const next = { ...search, listed: true, minPrice: f.from ?? undefined, maxPrice: f.to ?? undefined }
    setMin(f.from === null ? '' : String(f.from))
    setMax(f.to === null ? '' : String(f.to))
    setSearch(next)
  }

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
      <form className="search" role="search" onSubmit={submit}>
        <input
          aria-label="Search listings"
          placeholder="Search by word"
          value={text}
          maxLength={64}
          onChange={(e) => setText(e.target.value)}
        />
        <input aria-label="Lowest price" inputMode="numeric" placeholder="min" value={min} onChange={(e) => setMin(e.target.value)} />
        <input aria-label="Highest price" inputMode="numeric" placeholder="max" value={max} onChange={(e) => setMax(e.target.value)} />
        <select aria-label="Sort" value={sort} onChange={(e) => setSort(e.target.value as SearchSort)}>
          <option value="relevance">Best match</option>
          <option value="newest">Newest drawings</option>
          <option value="price_asc">Price: low to high</option>
          <option value="price_desc">Price: high to low</option>
        </select>
        <button type="submit">Search</button>
        {search && (
          <button
            type="button"
            onClick={() => {
              setSearch(null)
              setText('')
              setMin('')
              setMax('')
            }}
          >
            Clear
          </button>
        )}
      </form>
      {search && facets.length > 0 && (
        <p className="actions" aria-label="Price ranges">
          {facets.map((f) => (
            <button key={f.key} type="button" disabled={f.count === 0} onClick={() => byFacet(f)}>
              {f.key} coins ({f.count})
            </button>
          ))}
        </p>
      )}
      {error && <p role="alert">{error}</p>}
      {search ? (
        <SearchResults
          key={JSON.stringify(search)}
          query={search}
          onOpen={onOpen}
          onFacets={setFacets}
          extra={(hit) =>
            hit.listing && (
              <>
                <span className="price">{coins(hit.listing.price)}</span>
                {hit.listing.seller_id !== meId ? (
                  <BuyButton
                    listing={{
                      listing_id: hit.listing.listing_id,
                      drawing_id: hit.id,
                      seller_id: hit.listing.seller_id,
                      price: hit.listing.price,
                      status: 'ACTIVE',
                      buyer_id: null,
                      sale_id: null,
                      created_at: '',
                      closed_at: null,
                    }}
                    onBought={setBought}
                    onGone={() => {}}
                    onWallet={onWallet}
                  />
                ) : (
                  <span className="muted">Your listing</span>
                )}
              </>
            )
          }
        />
      ) : listings === null ? (
        !error && <p>Loading…</p>
      ) : listings.length === 0 ? (
        <p>Nothing is for sale right now. List one of your drawings from My drawings.</p>
      ) : (
        <ul className="gallery-grid">
          {listings.map((l) => (
            <li key={l.listing_id} className="market-card">
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
