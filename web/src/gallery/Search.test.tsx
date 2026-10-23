import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from '../auth.ts'
import type { SearchHit } from '../gallery.ts'
import { Market } from '../market/Market.tsx'
import { drawing, json, listing, mockApi } from '../market/testing.ts'
import { Gallery } from './Gallery.tsx'

const D1 = '00000000-0000-7000-8000-000000000001'
const D2 = '00000000-0000-7000-8000-000000000002'

function hit(id: string, word: string, price: number | null, seller = 'u_ada'): SearchHit {
  return {
    ...drawing(id, word),
    listing: price === null ? null : { listing_id: `l-${id.slice(-1)}`, price, seller_id: seller },
  }
}

const FACETS = {
  price: [
    { key: '1-10', from: 1, to: 10, count: 1 },
    { key: '11-50', from: 11, to: 50, count: 0 },
    { key: '51-200', from: 51, to: 200, count: 1 },
  ],
}

describe('Search', () => {
  beforeEach(() => resetAuthForTests())

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('searches the gallery by word', async () => {
    mockApi({
      'GET /api/assets/drawings?limit=24': () => json(200, { drawings: [drawing(D1, 'cat')], next_cursor: null }),
      'GET /api/assets/search?q=elephnt&limit=24': () =>
        json(200, { drawings: [hit(D2, 'elephant', null)], total: 1, facets: FACETS, next_cursor: null }),
    })
    render(<Gallery meId="u_me" onOpen={() => {}} onBack={() => {}} />)
    expect(await screen.findByText('cat')).toBeTruthy()

    fireEvent.change(screen.getByLabelText('Search drawings'), { target: { value: 'elephnt' } })
    fireEvent.click(screen.getByRole('button', { name: 'Search' }))

    expect(await screen.findByText('elephant')).toBeTruthy()
    expect(screen.getByText('1 drawing')).toBeTruthy()
    expect(screen.queryByText('cat')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Clear' }))
    expect(await screen.findByText('cat')).toBeTruthy()
  })

  it('searches listings with a price range, sorts, and narrows by a facet', async () => {
    const api = mockApi({
      'GET /api/market/listings?limit=24': () => json(200, { listings: [listing(1)], next_cursor: null }),
      [`GET /api/assets/drawings?ids=${listing(1).drawing_id}`]: () =>
        json(200, { drawings: [drawing(listing(1).drawing_id, 'cat')], next_cursor: null }),
      'GET /api/assets/search?q=cat&listed=true&max_price=200&sort=price_asc&limit=24': () =>
        json(200, {
          drawings: [hit(D1, 'cat', 5), hit(D2, 'catapult', 120, 'u_me')],
          total: 2,
          facets: FACETS,
          next_cursor: null,
        }),
      'GET /api/assets/search?q=cat&listed=true&min_price=1&max_price=10&sort=price_asc&limit=24': () =>
        json(200, { drawings: [hit(D1, 'cat', 5)], total: 1, facets: FACETS, next_cursor: null }),
    })
    render(<Market meId="u_me" onOpen={() => {}} onWallet={() => {}} />)
    await screen.findByText('cat')

    fireEvent.change(screen.getByLabelText('Search listings'), { target: { value: 'cat' } })
    fireEvent.change(screen.getByLabelText('Highest price'), { target: { value: '200' } })
    fireEvent.change(screen.getByLabelText('Sort'), { target: { value: 'price_asc' } })
    fireEvent.click(screen.getByRole('button', { name: 'Search' }))

    expect(await screen.findByText('catapult')).toBeTruthy()
    expect(screen.getByText('Your listing')).toBeTruthy()
    expect(screen.getAllByRole('button', { name: 'Buy' })).toHaveLength(1)
    // The 11-50 range has nothing, so it can't be picked; 1-10 narrows the search.
    expect((screen.getByRole('button', { name: '11-50 coins (0)' }) as HTMLButtonElement).disabled).toBe(true)
    fireEvent.click(screen.getByRole('button', { name: '1-10 coins (1)' }))
    expect(await screen.findByText('1 drawing')).toBeTruthy()
    expect(api.fetchMock.mock.calls.some(([u]) => String(u).includes('min_price=1&max_price=10'))).toBe(true)
  })

  it('says so when search is unavailable', async () => {
    mockApi({
      'GET /api/assets/drawings?limit=24': () => json(200, { drawings: [], next_cursor: null }),
      'GET /api/assets/search?q=cat&limit=24': () =>
        json(503, { error: { code: 'SEARCH_UNAVAILABLE', message: 'down' } }),
    })
    render(<Gallery meId="u_me" onOpen={() => {}} onBack={() => {}} />)
    fireEvent.change(await screen.findByLabelText('Search drawings'), { target: { value: 'cat' } })
    fireEvent.click(screen.getByRole('button', { name: 'Search' }))
    expect(await screen.findByText('Search is unavailable right now.')).toBeTruthy()
  })
})
