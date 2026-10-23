// The wallet: balance, transactions, and coin packs bought through Stripe Checkout.

import { useEffect, useState } from 'react'
import { ApiError } from '../api.ts'
import { useIdempotencyKeys } from '../idempotency.ts'
import {
  coins,
  getWallet,
  listCoinPacks,
  listTransactions,
  money,
  startCheckout,
  type CoinPack,
  type Transaction,
} from '../money.ts'

/** How often, and how long, the page checks for the coins after Stripe sends the player back. */
export const POLL_MS = 2_000
export const POLL_FOR_MS = 30_000

type Props = {
  /** "success" or "cancelled" when Stripe Checkout sent the player back here. */
  checkout: string | null
  /** Navigation to /wallet without the query, once the checkout message is no longer needed. */
  onClearCheckout: () => void
  /** Leaves the page; the browser leaves the app for Stripe's hosted page. */
  redirect?: (url: string) => void
}

type Arrival = 'waiting' | 'arrived' | 'late'

export function Wallet({ checkout, onClearCheckout, redirect = (url) => window.location.assign(url) }: Props) {
  const [balance, setBalance] = useState<number | null>(null)
  const [txs, setTxs] = useState<Transaction[] | null>(null)
  const [cursor, setCursor] = useState<string | null>(null)
  const [packs, setPacks] = useState<CoinPack[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [buying, setBuying] = useState<string | null>(null)
  const [arrival, setArrival] = useState<Arrival | null>(checkout === 'success' ? 'waiting' : null)
  const keys = useIdempotencyKeys()

  useEffect(() => {
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const failed = (e: unknown) => {
      if (!stopped) setError(e instanceof ApiError ? e.message : 'Could not load your wallet.')
    }
    listCoinPacks()
      .then((p) => !stopped && setPacks(p))
      .catch(failed)
    const show = (first: FirstPage) => {
      if (stopped) return
      setBalance(first.balance)
      setTxs(first.transactions)
      setCursor(first.nextCursor)
    }
    loadFirstPage()
      .then((first) => {
        show(first)
        const start = first.balance
        const newest = first.transactions[0]
        if (stopped || checkout !== 'success') return
        // The webhook may have credited the coins before this page loaded.
        if (newest?.type === 'stripe_purchase' && Date.now() - Date.parse(newest.created_at) < 5 * 60_000) {
          setArrival('arrived')
          return
        }
        const deadline = Date.now() + POLL_FOR_MS
        const poll = () => {
          timer = setTimeout(() => {
            loadFirstPage()
              .then((now) => {
                show(now)
                if (stopped) return
                if (now.balance > start) setArrival('arrived')
                else if (Date.now() >= deadline) setArrival('late')
                else poll()
              })
              .catch(() => !stopped && (Date.now() >= deadline ? setArrival('late') : poll()))
          }, POLL_MS)
        }
        poll()
      })
      .catch(failed)
    return () => {
      stopped = true
      clearTimeout(timer)
    }
  }, [checkout])

  function loadMore() {
    listTransactions(cursor)
      .then((page) => {
        setTxs((prev) => [...(prev ?? []), ...page.transactions])
        setCursor(page.next_cursor)
      })
      .catch((e: unknown) => setError(e instanceof ApiError ? e.message : 'Could not load more transactions.'))
  }

  function buy(pack: CoinPack) {
    setBuying(pack.pack_id)
    setError(null)
    keys
      .run(`checkout:${pack.pack_id}`, (key) => startCheckout(pack.pack_id, key))
      .then((c) => redirect(c.checkout_url))
      .catch((e: unknown) => {
        setBuying(null)
        setError(
          e instanceof ApiError
            ? `Could not start the payment: ${e.message}`
            : 'Could not reach the server. Press Buy again to retry the same payment.',
        )
      })
  }

  return (
    <main>
      <h1>Wallet</h1>
      {checkout === 'cancelled' && (
        <p role="status">
          Payment cancelled; nothing was charged.{' '}
          <button type="button" onClick={onClearCheckout}>
            OK
          </button>
        </p>
      )}
      {arrival === 'waiting' && <p role="status">Payment received. Waiting for Stripe to confirm it…</p>}
      {arrival === 'arrived' && <p role="status" className="notice good">Your coins have arrived.</p>}
      {arrival === 'late' && (
        <p role="status">
          Your coins haven't arrived yet. They are credited when Stripe confirms the payment, which can take a little
          longer; refresh this page in a minute.
        </p>
      )}
      {error && <p role="alert">{error}</p>}

      <p className="balance">{balance === null ? 'Loading…' : coins(balance)}</p>

      <h2>Buy coins</h2>
      {packs === null ? (
        !error && <p>Loading…</p>
      ) : (
        <ul className="pack-list">
          {packs.map((p) => (
            <li key={p.pack_id}>
              <span>{coins(p.coins)}</span>
              <span>{money(p.price_cents, p.currency)}</span>
              <button type="button" disabled={buying !== null} onClick={() => buy(p)}>
                {buying === p.pack_id ? 'Opening Stripe…' : 'Buy'}
              </button>
            </li>
          ))}
        </ul>
      )}
      <p className="muted">Payments run in Stripe's test mode: use a Stripe test card, never a real one.</p>

      <h2>Transactions</h2>
      {txs === null ? null : txs.length === 0 ? (
        <p>No transactions yet. Play a match to earn coins.</p>
      ) : (
        <table className="ledger">
          <tbody>
            {txs.map((t) => (
              <tr key={`${t.tx_id}:${t.amount}`}>
                <td>{new Date(t.created_at).toLocaleString()}</td>
                <td>{describe(t)}</td>
                <td className={t.amount < 0 ? 'debit' : 'credit'}>
                  {t.amount > 0 ? '+' : ''}
                  {t.amount.toLocaleString('en-US')}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {cursor && (
        <button type="button" onClick={loadMore}>
          Load more
        </button>
      )}
    </main>
  )
}

type FirstPage = { balance: number; transactions: Transaction[]; nextCursor: string | null }

async function loadFirstPage(): Promise<FirstPage> {
  const [wallet, page] = await Promise.all([getWallet(), listTransactions()])
  return { balance: wallet.balance, transactions: page.transactions, nextCursor: page.next_cursor }
}

function describe(t: Transaction): string {
  switch (t.type) {
    case 'reward':
      return 'Match reward'
    case 'stripe_purchase':
      return 'Coin pack'
    case 'sale':
      return t.amount < 0 ? 'Bought a drawing' : 'Drawing sale or royalty'
    case 'tip':
      return 'Tip to a drawer'
    case 'tip_settlement':
      return 'Tips for your drawing'
    case 'tip_refund':
      return 'Tip returned'
    default:
      return t.type
  }
}
