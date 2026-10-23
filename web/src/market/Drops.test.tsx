import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from '../auth.ts'
import type { Drop, QueueStatus } from '../money.ts'
import { countdown } from './countdown.ts'
import { Drops } from './Drops.tsx'
import { drawing, json, mockApi, type Route } from './testing.ts'

const D1 = '00000000-0000-7000-8000-000000000001'
const DROP = 'd0000000-0000-7000-8000-000000000001'

function drop(over: Partial<Drop> = {}): Drop {
  return {
    drop_id: DROP,
    drawing_id: D1,
    editions: 3,
    editions_left: 3,
    price: 20,
    opens_at: new Date(Date.now() - 1000).toISOString(),
    closes_at: new Date(Date.now() + 3_600_000).toISOString(),
    status: 'OPEN',
    ...over,
  }
}

function queue(over: Partial<QueueStatus> = {}): QueueStatus {
  return {
    drop_id: DROP,
    position: 0,
    admitted: true,
    waiting: 5,
    editions_left: 3,
    opens_at: '',
    status: 'OPEN',
    ...over,
  }
}

describe('Drops', () => {
  beforeEach(() => resetAuthForTests())

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  function serve(d: Drop, routes: Record<string, Route> = {}) {
    return mockApi({
      'GET /api/market/drops': () => json(200, { drops: [d] }),
      [`GET /api/assets/drawings?ids=${D1}`]: () => json(200, { drawings: [drawing(D1, 'cat')], next_cursor: null }),
      ...routes,
    })
  }

  it('joins an open drop and buys one edition with an Idempotency-Key', async () => {
    const api = serve(drop(), {
      [`POST /api/market/drops/${DROP}/join`]: () => json(200, queue()),
      [`POST /api/market/drops/${DROP}/purchase`]: () =>
        json(201, { drop_id: DROP, drawing_id: D1, edition_no: 2, editions: 3, price: 20, purchased_at: '' }),
    })
    render(<Drops isAdmin={false} onOpen={() => {}} onWallet={() => {}} />)

    expect(await screen.findByText('cat')).toBeTruthy()
    expect(screen.getByText(/3 of 3 left/)).toBeTruthy()
    expect(screen.queryByRole('form', { name: 'Create a drop' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Join the waiting room' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Buy for 20 coins' }))

    expect(await screen.findByText('You got edition #2 of 3.')).toBeTruthy()
    expect(api.keys(`POST /api/market/drops/${DROP}/purchase`)[0]).toBeTruthy()
  })

  it('waits for the lottery before the open and shows the place after it', async () => {
    serve(drop({ opens_at: new Date(Date.now() + 90_000).toISOString(), status: 'UPCOMING' }), {
      [`POST /api/market/drops/${DROP}/join`]: () => json(200, queue({ admitted: false, position: 4 })),
    })
    render(<Drops isAdmin={false} onOpen={() => {}} onWallet={() => {}} />)

    expect(await screen.findByText(/Opens in 1:(29|30)/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Join the waiting room' }))
    expect(await screen.findByText("You're in. Places are drawn when the drop opens.")).toBeTruthy()
  })

  it('shows the place in line once open, and a sold-out refusal in words', async () => {
    serve(drop(), {
      [`POST /api/market/drops/${DROP}/join`]: () => json(200, queue({ admitted: false, position: 7 })),
    })
    render(<Drops isAdmin={false} onOpen={() => {}} onWallet={() => {}} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Join the waiting room' }))
    expect(await screen.findByText(/You're in line: 7 places ahead of you/)).toBeTruthy()
    cleanup()

    serve(drop(), {
      [`POST /api/market/drops/${DROP}/join`]: () => json(200, queue()),
      [`POST /api/market/drops/${DROP}/purchase`]: () =>
        json(409, { error: { code: 'SOLD_OUT', message: 'every edition is taken' } }),
    })
    render(<Drops isAdmin={false} onOpen={() => {}} onWallet={() => {}} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Join the waiting room' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Buy for 20 coins' }))
    expect(await screen.findByText('Every edition is taken.')).toBeTruthy()
  })

  it('lets an admin create a drop with an Idempotency-Key', async () => {
    let body: Record<string, unknown> = {}
    const api = mockApi({
      'GET /api/market/drops': () => json(200, { drops: [] }),
      'POST /api/market/drops': (init) => {
        body = JSON.parse(String(init.body)) as Record<string, unknown>
        return json(201, drop({ status: 'UPCOMING' }))
      },
      [`GET /api/assets/drawings?ids=${D1}`]: () => json(200, { drawings: [drawing(D1, 'cat')], next_cursor: null }),
    })
    render(<Drops isAdmin onOpen={() => {}} onWallet={() => {}} />)

    fireEvent.change(await screen.findByLabelText('Drawing id'), { target: { value: D1 } })
    fireEvent.change(screen.getByLabelText('Editions'), { target: { value: '5' } })
    fireEvent.click(screen.getByRole('button', { name: 'Create the drop' }))

    expect(await screen.findByText('cat')).toBeTruthy()
    expect(body).toMatchObject({ drawing_id: D1, editions: 5, price: 25, duration_minutes: 60 })
    expect(Date.parse(String(body.opens_at))).toBeGreaterThan(Date.now())
    expect(api.keys('POST /api/market/drops')[0]).toBeTruthy()
  })

  it('counts down in minutes and seconds, or hours', () => {
    expect(countdown(65_000)).toBe('1:05')
    expect(countdown(0)).toBe('0:00')
    expect(countdown(2 * 3_600_000 + 3 * 60_000)).toBe('2 h 3 min')
  })
})
