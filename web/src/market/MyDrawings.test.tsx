import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from '../auth.ts'
import type { OwnedDrawing } from '../money.ts'
import { MyDrawings } from './MyDrawings.tsx'
import { drawing, json, listing, mockApi } from './testing.ts'

const D1 = '00000000-0000-7000-8000-000000000001'
const D2 = '00000000-0000-7000-8000-000000000002'

function owned(id: string, over: Partial<OwnedDrawing> = {}): OwnedDrawing {
  return { drawing_id: id, version: 1, minted_at: '', updated_at: '', listing: null, ...over }
}

describe('MyDrawings', () => {
  beforeEach(() => resetAuthForTests())

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  function serve(extra: Record<string, (init: RequestInit) => Response> = {}) {
    return mockApi({
      'GET /api/market/drawings?limit=24&owner=me': () =>
        json(200, {
          drawings: [owned(D1), owned(D2, { listing: { listing_id: 'l2', price: 80, created_at: '' } })],
          next_cursor: null,
        }),
      [`GET /api/assets/drawings?ids=${D1},${D2}`]: () =>
        json(200, { drawings: [drawing(D1, 'cat'), drawing(D2, 'dog')], next_cursor: null }),
      ...extra,
    })
  }

  it('lists a drawing for sale with an Idempotency-Key', async () => {
    let body: unknown
    const api = serve({
      'POST /api/market/listings': (init) => {
        body = JSON.parse(String(init.body))
        return json(201, listing(1, { listing_id: 'l9', drawing_id: D1, seller_id: 'u_me', price: 120 }))
      },
    })
    render(<MyDrawings onOpen={() => {}} />)

    expect(await screen.findByText('cat')).toBeTruthy()
    expect(screen.getByText('Listed for 80 coins')).toBeTruthy()
    fireEvent.change(screen.getByLabelText('Price in coins'), { target: { value: '120' } })
    fireEvent.click(screen.getByRole('button', { name: 'List for sale' }))

    expect(await screen.findByText('Listed for 120 coins')).toBeTruthy()
    expect(body).toEqual({ drawing_id: D1, price: 120 })
    expect(api.keys('POST /api/market/listings')[0]).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('refuses a price that is not a whole number of coins', async () => {
    const api = serve()
    render(<MyDrawings onOpen={() => {}} />)

    for (const bad of ['0', '1.5', 'abc', '1000001']) {
      fireEvent.change(await screen.findByLabelText('Price in coins'), { target: { value: bad } })
      fireEvent.click(screen.getByRole('button', { name: 'List for sale' }))
      expect(await screen.findByText(/Enter a whole number of coins/)).toBeTruthy()
    }
    expect(api.keys('POST /api/market/listings')).toHaveLength(0)
  })

  it('cancels a listing', async () => {
    serve({
      'DELETE /api/market/listings/l2': () => json(200, listing(2, { listing_id: 'l2', status: 'CANCELLED' })),
    })
    render(<MyDrawings onOpen={() => {}} />)

    fireEvent.click(await screen.findByRole('button', { name: 'Cancel listing' }))
    await vi.waitFor(() => expect(screen.getAllByRole('button', { name: 'List for sale' })).toHaveLength(2))
  })

  it('says when there is nothing yet', async () => {
    mockApi({ 'GET /api/market/drawings?limit=24&owner=me': () => json(200, { drawings: [], next_cursor: null }) })
    render(<MyDrawings onOpen={() => {}} />)
    expect(await screen.findByText(/You don't own any drawings yet/)).toBeTruthy()
  })
})
