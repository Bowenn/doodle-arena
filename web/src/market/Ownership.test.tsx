import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from '../auth.ts'
import type { OwnershipHistory } from '../money.ts'
import { Ownership } from './Ownership.tsx'
import { json, listing, mockApi, sale } from './testing.ts'

const ID = '01a14d4b-278f-74a1-a0be-62cc85301740'
const HISTORY: OwnershipHistory = {
  drawing_id: ID,
  owner_id: 'u_bob12345678',
  version: 2,
  author_ids: ['u_ada12345678'],
  events: [
    { kind: 'MINT', from_owner: null, to_owner: 'u_ada12345678', sale_id: null, price: null, created_at: '2026-10-18T10:00:00Z' },
    { kind: 'SALE', from_owner: 'u_ada12345678', to_owner: 'u_bob12345678', sale_id: 's1', price: 60, created_at: '2026-10-18T11:00:00Z' },
  ],
}
const HISTORY_URL = `GET /api/market/drawings/${ID}/ownership-history`
const LISTING_URL = `GET /api/market/listings?limit=24&drawing_id=${ID}`

describe('Ownership', () => {
  beforeEach(() => resetAuthForTests())

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('shows the owner and the provenance with sale prices', async () => {
    mockApi({
      [HISTORY_URL]: () => json(200, HISTORY),
      [LISTING_URL]: () => json(200, { listings: [], next_cursor: null }),
    })
    render(<Ownership drawingId={ID} meId="u_me" onWallet={() => {}} />)

    expect(await screen.findByText('Player u_bob123')).toBeTruthy()
    expect(screen.getByText('Minted to Player u_ada123')).toBeTruthy()
    expect(screen.getByText('Sold by Player u_ada123 to Player u_bob123 for 60 coins')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Buy' })).toBeNull()
  })

  it('offers Buy when listed by someone else, and reloads after the purchase', async () => {
    const l = listing(1, { drawing_id: ID, seller_id: 'u_bob12345678', price: 90 })
    let bought = false
    mockApi({
      [HISTORY_URL]: () =>
        json(200, bought ? { ...HISTORY, owner_id: 'u_me', version: 3 } : HISTORY),
      [LISTING_URL]: () => json(200, { listings: bought ? [] : [l], next_cursor: null }),
      'POST /api/market/listings/l1/purchase': () => {
        bought = true
        return json(200, sale(l))
      },
    })
    render(<Ownership drawingId={ID} meId="u_me" onWallet={() => {}} />)

    expect(await screen.findByText('90 coins')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Buy' }))
    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }))
    expect(await screen.findByText('you')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Buy' })).toBeNull()
  })

  it('hides Buy on my own listing', async () => {
    mockApi({
      [HISTORY_URL]: () => json(200, { ...HISTORY, owner_id: 'u_me' }),
      [LISTING_URL]: () => json(200, { listings: [listing(1, { drawing_id: ID, seller_id: 'u_me' })], next_cursor: null }),
    })
    render(<Ownership drawingId={ID} meId="u_me" onWallet={() => {}} />)
    expect(await screen.findByText('60 coins')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Buy' })).toBeNull()
  })

  it('says a drawing without an owner yet is not tradable', async () => {
    mockApi({
      [HISTORY_URL]: () => json(404, { error: { code: 'DRAWING_NOT_FOUND', message: 'no such drawing' } }),
      [LISTING_URL]: () => json(200, { listings: [], next_cursor: null }),
    })
    render(<Ownership drawingId={ID} meId="u_me" onWallet={() => {}} />)
    expect(await screen.findByText("This drawing isn't tradable yet.")).toBeTruthy()
  })
})
