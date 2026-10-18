import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from '../auth.ts'
import type { Transaction } from '../money.ts'
import { POLL_FOR_MS, POLL_MS, Wallet } from './Wallet.tsx'

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function tx(n: number, over: Partial<Transaction> = {}): Transaction {
  return { tx_id: `t${n}`, type: 'reward', amount: 30, created_at: '2026-10-18T10:00:00Z', ...over }
}

const PACKS = { packs: [{ pack_id: 'small', coins: 500, price_cents: 499, currency: 'usd' }] }

describe('Wallet', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  let balance: number
  let pages: Record<string, { transactions: Transaction[]; next_cursor: string | null }>
  let checkout: () => Promise<Response>

  beforeEach(() => {
    resetAuthForTests()
    balance = 130
    pages = { '/api/market/wallet/transactions?limit=20': { transactions: [tx(2), tx(1, { amount: 100 })], next_cursor: null } }
    checkout = () => Promise.resolve(json(200, { session_id: 'cs_1', checkout_url: 'https://checkout.stripe.test/cs_1' }))
    fetchMock = vi.fn((url: string) => {
      if (url === '/api/auth/refresh') {
        return Promise.resolve(json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 }))
      }
      if (url === '/api/market/wallet') return Promise.resolve(json(200, { user_id: 'u_me', balance, updated_at: null }))
      if (url === '/api/market/coin-packs') return Promise.resolve(json(200, PACKS))
      if (url === '/api/market/coin-packs/small/checkout') return checkout()
      const page = pages[url]
      return Promise.resolve(page ? json(200, page) : json(500, { error: { code: 'INTERNAL', message: 'boom' } }))
    })
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    cleanup()
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  function headersOf(url: string): Headers[] {
    return fetchMock.mock.calls.filter(([u]) => u === url).map(([, init]) => new Headers((init as RequestInit).headers))
  }

  it('shows the balance, the coin packs and the history, and loads more', async () => {
    pages['/api/market/wallet/transactions?limit=20'].next_cursor = 'c1'
    pages['/api/market/wallet/transactions?cursor=c1&limit=20'] = {
      transactions: [tx(0, { type: 'sale', amount: -60 })],
      next_cursor: null,
    }
    render(<Wallet checkout={null} onClearCheckout={() => {}} />)

    expect(await screen.findByText('130 coins')).toBeTruthy()
    expect(screen.getByText('500 coins')).toBeTruthy()
    expect(screen.getByText('$4.99')).toBeTruthy()
    expect(screen.getAllByText('Match reward')).toHaveLength(2)
    fireEvent.click(screen.getByRole('button', { name: 'Load more' }))
    expect(await screen.findByText('Bought a drawing')).toBeTruthy()
    expect(screen.getByText('-60')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull()
  })

  it('opens Stripe Checkout with an Idempotency-Key', async () => {
    const redirect = vi.fn()
    render(<Wallet checkout={null} onClearCheckout={() => {}} redirect={redirect} />)

    fireEvent.click(await screen.findByRole('button', { name: 'Buy' }))
    await vi.waitFor(() => expect(redirect).toHaveBeenCalledWith('https://checkout.stripe.test/cs_1'))
    const [headers] = headersOf('/api/market/coin-packs/small/checkout')
    expect(headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('retries a checkout that never got an answer with the same key', async () => {
    checkout = () => Promise.reject(new TypeError('Failed to fetch'))
    const redirect = vi.fn()
    render(<Wallet checkout={null} onClearCheckout={() => {}} redirect={redirect} />)

    fireEvent.click(await screen.findByRole('button', { name: 'Buy' }))
    expect(await screen.findByText(/Press Buy again to retry the same payment/)).toBeTruthy()
    checkout = () => Promise.resolve(json(200, { session_id: 'cs_1', checkout_url: 'https://checkout.stripe.test/cs_1' }))
    fireEvent.click(screen.getByRole('button', { name: 'Buy' }))
    await vi.waitFor(() => expect(redirect).toHaveBeenCalled())

    const keys = headersOf('/api/market/coin-packs/small/checkout').map((h) => h.get('Idempotency-Key'))
    expect(keys).toHaveLength(2)
    expect(keys[1]).toBe(keys[0])
  })

  it('after a payment, polls until the coins arrive', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'], shouldAdvanceTime: true })
    render(<Wallet checkout="success" onClearCheckout={() => {}} />)

    expect(await screen.findByText(/Waiting for Stripe to confirm it/)).toBeTruthy()
    await vi.advanceTimersByTimeAsync(POLL_MS)
    expect(screen.getByText(/Waiting for Stripe/)).toBeTruthy()
    balance = 630
    await vi.advanceTimersByTimeAsync(POLL_MS)
    expect(await screen.findByText('Your coins have arrived.')).toBeTruthy()
    expect(screen.getByText('630 coins')).toBeTruthy()
  })

  it('says so when the coins are late', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'], shouldAdvanceTime: true })
    render(<Wallet checkout="success" onClearCheckout={() => {}} />)

    expect(await screen.findByText(/Waiting for Stripe/)).toBeTruthy()
    await vi.advanceTimersByTimeAsync(POLL_FOR_MS + POLL_MS)
    expect(await screen.findByText(/Your coins haven't arrived yet/)).toBeTruthy()
  })

  it('knows the coins already arrived when the webhook beat the redirect', async () => {
    pages['/api/market/wallet/transactions?limit=20'].transactions = [
      tx(3, { type: 'stripe_purchase', amount: 500, created_at: new Date().toISOString() }),
    ]
    render(<Wallet checkout="success" onClearCheckout={() => {}} />)
    expect(await screen.findByText('Your coins have arrived.')).toBeTruthy()
    expect(screen.getByText('Coin pack')).toBeTruthy()
  })

  it('says a cancelled payment charged nothing', async () => {
    const clear = vi.fn()
    render(<Wallet checkout="cancelled" onClearCheckout={clear} />)
    expect(await screen.findByText(/Payment cancelled; nothing was charged/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'OK' }))
    expect(clear).toHaveBeenCalled()
  })
})
