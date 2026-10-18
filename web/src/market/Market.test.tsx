import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from '../auth.ts'
import { Market } from './Market.tsx'
import { drawing, json, listing, mockApi, sale, type Route } from './testing.ts'

const L1 = listing(1)
const L2 = listing(2, { seller_id: 'u_me', price: 80 })
const LISTINGS = 'GET /api/market/listings?limit=24'
const BATCH = `GET /api/assets/drawings?ids=${L1.drawing_id},${L2.drawing_id}`
const BUY = 'POST /api/market/listings/l1/purchase'

describe('Market', () => {
  let routes: Record<string, Route>

  beforeEach(() => {
    resetAuthForTests()
    routes = {
      [LISTINGS]: () => json(200, { listings: [L1, L2], next_cursor: null }),
      [BATCH]: () => json(200, { drawings: [drawing(L1.drawing_id, 'cat'), drawing(L2.drawing_id, 'dog')], next_cursor: null }),
      [BUY]: () => json(200, sale(L1)),
    }
  })

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  async function buyCat() {
    fireEvent.click(await screen.findByRole('button', { name: 'Buy' }))
    expect(screen.getByText('Buy for 60 coins?')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }))
  }

  it('shows listings with thumbnails from one batch lookup, and no Buy on my own', async () => {
    const api = mockApi(routes)
    const open = vi.fn()
    render(<Market meId="u_me" onOpen={open} onWallet={() => {}} />)

    expect(await screen.findByText('cat')).toBeTruthy()
    expect(screen.getByAltText('A drawing of "dog"').getAttribute('src')).toBe('http://s3/dog-thumb.png')
    expect(screen.getByText('Sold by Player u_ada')).toBeTruthy()
    expect(screen.getByText('Sold by you')).toBeTruthy()
    expect(screen.getAllByRole('button', { name: 'Buy' })).toHaveLength(1)
    expect(api.fetchMock.mock.calls.filter(([u]) => String(u).startsWith('/api/assets/'))).toHaveLength(1)
    fireEvent.click(screen.getByText('cat'))
    expect(open).toHaveBeenCalledWith(L1.drawing_id)
  })

  it('buys after a confirmation, with an Idempotency-Key', async () => {
    const api = mockApi(routes)
    render(<Market meId="u_me" onOpen={() => {}} onWallet={() => {}} />)

    await buyCat()
    expect(await screen.findByText(/You bought “cat” for 60 coins/)).toBeTruthy()
    expect(screen.queryByText('Sold by Player u_ada')).toBeNull()
    expect(api.keys(BUY)[0]).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('says when someone else bought it first', async () => {
    routes[BUY] = () => json(409, { error: { code: 'LISTING_NOT_AVAILABLE', message: 'not available' } })
    mockApi(routes)
    render(<Market meId="u_me" onOpen={() => {}} onWallet={() => {}} />)

    await buyCat()
    expect(await screen.findByText(/Someone else just bought it/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull()
  })

  it('links to the wallet when coins are short', async () => {
    routes[BUY] = () => json(422, { error: { code: 'INSUFFICIENT_FUNDS', message: 'insufficient funds' } })
    mockApi(routes)
    const wallet = vi.fn()
    render(<Market meId="u_me" onOpen={() => {}} onWallet={wallet} />)

    await buyCat()
    expect(await screen.findByText('You need 60 coins.')).toBeTruthy()
    fireEvent.click(screen.getByRole('link', { name: 'Get coins' }))
    expect(wallet).toHaveBeenCalled()
  })

  it('retries a purchase that got no answer with the same key', async () => {
    let calls = 0
    routes[BUY] = () => {
      calls++
      if (calls === 1) throw new TypeError('Failed to fetch')
      return json(200, sale(L1))
    }
    const api = mockApi(routes)
    render(<Market meId="u_me" onOpen={() => {}} onWallet={() => {}} />)

    await buyCat()
    expect(await screen.findByText('Could not reach the server.')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByText(/You bought “cat”/)).toBeTruthy()
    const keys = api.keys(BUY)
    expect(keys).toHaveLength(2)
    expect(keys[1]).toBe(keys[0])
  })
})
