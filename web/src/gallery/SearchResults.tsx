// Search results (ADR 0023), shared by the Gallery and the Market: a grid of hits with "Load
// more", the total, and the price facet's counts. Remounted (by key) when the query changes.

import { useEffect, useState, type ReactNode } from 'react'
import { ApiError } from '../api.ts'
import { searchDrawings, type PriceFacet, type SearchHit, type SearchQuery } from '../gallery.ts'

type Props = {
  query: SearchQuery
  onOpen: (drawingId: string) => void
  /** What a card shows under the drawing (the Market: price and Buy). */
  extra?: (hit: SearchHit) => ReactNode
  /** The price facet's counts, when the caller shows them. */
  onFacets?: (facets: PriceFacet[]) => void
}

export function SearchResults({ query, onOpen, extra, onFacets }: Props) {
  const [hits, setHits] = useState<SearchHit[] | null>(null)
  const [total, setTotal] = useState(0)
  const [cursor, setCursor] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [query0] = useState(query)

  useEffect(() => {
    searchDrawings(query0)
      .then((page) => {
        setHits(page.drawings)
        setTotal(page.total)
        setCursor(page.next_cursor)
        onFacets?.(page.facets.price)
      })
      .catch((e: unknown) =>
        setError(
          e instanceof ApiError && e.code === 'SEARCH_UNAVAILABLE'
            ? 'Search is unavailable right now.'
            : 'Could not search.',
        ),
      )
    // onFacets is the caller's setter; the query is fixed for this mount.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query0])

  function loadMore() {
    searchDrawings({ ...query0, cursor })
      .then((page) => {
        setHits((prev) => [...(prev ?? []), ...page.drawings])
        setCursor(page.next_cursor)
      })
      .catch(() => setError('Could not load more.'))
  }

  if (error) return <p role="alert">{error}</p>
  if (hits === null) return <p>Searching…</p>
  return (
    <>
      <p className="muted" role="status">
        {total === 0 ? 'Nothing matches.' : `${total} ${total === 1 ? 'drawing' : 'drawings'}`}
      </p>
      <ul className="gallery-grid">
        {hits.map((h) => (
          <li key={h.id} className="market-card">
            <a
              href={`/drawings/${h.id}`}
              className="drawing-card"
              onClick={(e) => {
                e.preventDefault()
                onOpen(h.id)
              }}
            >
              {h.thumb_url ? (
                <img src={h.thumb_url} alt={`A drawing of "${h.word}"`} width={256} height={192} loading="lazy" />
              ) : (
                <span className="thumb-placeholder">No image yet</span>
              )}
              <span className="drawing-word">{h.word}</span>
            </a>
            {extra?.(h)}
          </li>
        ))}
      </ul>
      {cursor && (
        <button type="button" onClick={loadMore}>
          Load more
        </button>
      )}
    </>
  )
}
