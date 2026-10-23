import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from '../auth.ts'
import { json, mockApi, type Route } from '../market/testing.ts'
import { TipBox } from './TipBox.tsx'

const ROUND = { roomId: 'r1', matchId: 'm1', roundNo: 2, drawers: 'Ada', open: true }

function tipped(init: RequestInit): Response {
  const body = JSON.parse(String(init.body)) as { amount: number }
  return json(201, {
    tip_id: 't1',
    room_id: 'r1',
    match_id: 'm1',
    round_no: 2,
    amount: body.amount,
    status: 'PENDING',
    created_at: '',
  })
}

describe('TipBox', () => {
  let tipRoute: Route

  beforeEach(() => {
    resetAuthForTests()
    tipRoute = tipped
  })

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  function api() {
    return mockApi({
      'GET /api/market/wallet': () => json(200, { user_id: 'u_me', balance: 40, updated_at: null }),
      'POST /api/market/tips': (init) => tipRoute(init),
    })
  }

  it('tips the round with a key and takes the coins off the shown balance', async () => {
    const { fetchMock, keys } = api()
    render(<TipBox {...ROUND} />)
    expect(await screen.findByText(/You have 40 coins/)).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Tip 10' }))

    expect(await screen.findByText('You tipped 10 coins. It reaches Ada when the round ends.')).toBeTruthy()
    expect(screen.getByText(/You have 30 coins/)).toBeTruthy()
    const call = fetchMock.mock.calls.find(([u]) => u === '/api/market/tips')
    expect(JSON.parse(String(call?.[1]?.body))).toEqual({ room_id: 'r1', match_id: 'm1', round_no: 2, amount: 10 })
    expect(keys('POST /api/market/tips')[0]).toBeTruthy()
  })

  it('reuses the key when the first attempt never got an answer', async () => {
    let calls = 0
    tipRoute = (init) => {
      calls += 1
      if (calls === 1) throw new TypeError('network down')
      return tipped(init)
    }
    const { keys } = api()
    render(<TipBox {...ROUND} />)

    fireEvent.click(await screen.findByRole('button', { name: 'Tip 5' }))
    expect(await screen.findByText('Could not reach the server. Try again.')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Tip 5' }))
    expect(await screen.findByText(/You tipped 5 coins/)).toBeTruthy()

    const sent = keys('POST /api/market/tips')
    expect(sent).toHaveLength(2)
    expect(sent[1]).toBe(sent[0])
  })

  it('explains a tip beyond the wallet and a round that is over', async () => {
    tipRoute = () => json(422, { error: { code: 'INSUFFICIENT_FUNDS', message: 'no' } })
    api()
    render(<TipBox {...ROUND} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Tip 50' }))
    expect(await screen.findByText("You don't have 50 coins.")).toBeTruthy()

    tipRoute = () => json(409, { error: { code: 'ROUND_OVER', message: 'over' } })
    fireEvent.click(screen.getByRole('button', { name: 'Tip 5' }))
    expect(await screen.findByText('This round is over. Tip the next one.')).toBeTruthy()
  })

  it('is closed between rounds', async () => {
    api()
    render(<TipBox {...ROUND} open={false} />)
    expect(await screen.findByText(/Tips open when the next round starts/)).toBeTruthy()
    expect((screen.getByRole('button', { name: 'Tip 5' }) as HTMLButtonElement).disabled).toBe(true)
  })
})
