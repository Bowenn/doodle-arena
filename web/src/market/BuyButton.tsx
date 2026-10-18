// Buying one listing: confirm, then purchase with an Idempotency-Key, and explain each outcome.

import { useState } from 'react'
import { ApiError } from '../api.ts'
import { useIdempotencyKeys } from '../idempotency.ts'
import { coins, purchase, type Listing, type Sale } from '../money.ts'

type Props = {
  listing: Listing
  onBought: (sale: Sale) => void
  /** The listing is gone (sold to someone else or cancelled): the caller drops it. */
  onGone: () => void
  onWallet: () => void
}

type State =
  | { kind: 'idle' }
  | { kind: 'confirming' }
  | { kind: 'buying' }
  | { kind: 'failed'; message: string; retry: boolean; wallet?: boolean }

export function BuyButton({ listing, onBought, onGone, onWallet }: Props) {
  const [state, setState] = useState<State>({ kind: 'idle' })
  const keys = useIdempotencyKeys()

  function buy() {
    setState({ kind: 'buying' })
    keys
      .run(`buy:${listing.listing_id}`, (key) => purchase(listing.listing_id, key))
      .then((sale) => {
        setState({ kind: 'idle' })
        onBought(sale)
      })
      .catch((e: unknown) => {
        if (!(e instanceof ApiError)) {
          setState({ kind: 'failed', message: 'Could not reach the server.', retry: true })
        } else if (e.code === 'LISTING_NOT_AVAILABLE' || e.code === 'LISTING_NOT_FOUND') {
          setState({ kind: 'failed', message: 'Someone else just bought it, or the seller took it back.', retry: false })
          onGone()
        } else if (e.code === 'INSUFFICIENT_FUNDS') {
          setState({ kind: 'failed', message: `You need ${coins(listing.price)}.`, retry: false, wallet: true })
        } else if (e.code === 'IDEMPOTENCY_KEY_IN_PROGRESS' || e.status >= 500) {
          setState({ kind: 'failed', message: 'The purchase is still being processed.', retry: true })
        } else {
          setState({ kind: 'failed', message: e.message, retry: false })
        }
      })
  }

  switch (state.kind) {
    case 'idle':
      return (
        <button type="button" onClick={() => setState({ kind: 'confirming' })}>
          Buy
        </button>
      )
    case 'confirming':
      return (
        <span className="actions">
          <span>Buy for {coins(listing.price)}?</span>
          <button type="button" onClick={buy}>
            Confirm
          </button>
          <button type="button" onClick={() => setState({ kind: 'idle' })}>
            Cancel
          </button>
        </span>
      )
    case 'buying':
      return (
        <button type="button" disabled>
          Buying…
        </button>
      )
    case 'failed':
      return (
        <span className="actions">
          <span role="alert" className="notice bad">
            {state.message}
          </span>
          {state.wallet && (
            <a
              href="/wallet"
              onClick={(e) => {
                e.preventDefault()
                onWallet()
              }}
            >
              Get coins
            </a>
          )}
          {state.retry && (
            <button type="button" onClick={buy}>
              Try again
            </button>
          )}
        </span>
      )
  }
}
