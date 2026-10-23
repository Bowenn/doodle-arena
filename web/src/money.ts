// Marketplace REST client (design doc, section 6.3): wallet, coin packs, listings, purchases,
// owned drawings, ownership history and tips. Amounts are whole coins; prices in cents only for Stripe.

import { call } from './api.ts'

export type Wallet = { user_id: string; balance: number; updated_at: string | null }

export type TransactionType = 'reward' | 'stripe_purchase' | 'sale' | 'tip' | 'tip_settlement' | 'tip_refund'

/** One wallet entry: amount is signed (+ credited, - debited). */
export type Transaction = { tx_id: string; type: TransactionType; amount: number; created_at: string }

export type TransactionPage = { transactions: Transaction[]; next_cursor: string | null }

export type CoinPack = { pack_id: string; coins: number; price_cents: number; currency: string }

export type Checkout = { session_id: string; checkout_url: string }

export type Listing = {
  listing_id: string
  drawing_id: string
  seller_id: string
  price: number
  status: 'ACTIVE' | 'SOLD' | 'CANCELLED'
  buyer_id: string | null
  sale_id: string | null
  created_at: string
  closed_at: string | null
}

export type ListingPage = { listings: Listing[]; next_cursor: string | null }

export type Sale = {
  sale_id: string
  listing_id: string
  drawing_id: string
  seller_id: string
  buyer_id: string
  price: number
  royalties: { user_id: string; amount: number }[]
  completed_at: string
}

export type OwnedDrawing = {
  drawing_id: string
  version: number
  minted_at: string
  updated_at: string
  listing: { listing_id: string; price: number; created_at: string } | null
}

export type OwnedPage = { drawings: OwnedDrawing[]; next_cursor: string | null }

export type OwnershipEvent = {
  kind: 'MINT' | 'SALE'
  from_owner: string | null
  to_owner: string
  sale_id: string | null
  price: number | null
  created_at: string
}

export type OwnershipHistory = {
  drawing_id: string
  owner_id: string
  version: number
  author_ids: string[]
  events: OwnershipEvent[]
}

/** A tip in escrow until its round ends (ADR 0021). */
export type Tip = {
  tip_id: string
  room_id: string
  match_id: string
  round_no: number
  amount: number
  status: 'PENDING' | 'SETTLED' | 'REFUNDED'
  created_at: string
}

/** The largest listing price the web app offers (the server only needs a positive integer). */
export const MAX_PRICE = 1_000_000

function idem(key: string): Record<string, string> {
  return { 'Idempotency-Key': key }
}

function page(cursor: string | null | undefined, limit: number): URLSearchParams {
  const params = new URLSearchParams()
  if (cursor) params.set('cursor', cursor)
  params.set('limit', String(limit))
  return params
}

export function getWallet(): Promise<Wallet> {
  return call('GET', '/api/market/wallet')
}

export function listTransactions(cursor?: string | null): Promise<TransactionPage> {
  return call('GET', `/api/market/wallet/transactions?${page(cursor, 20).toString()}`)
}

export async function listCoinPacks(): Promise<CoinPack[]> {
  return (await call<{ packs: CoinPack[] }>('GET', '/api/market/coin-packs')).packs
}

export function startCheckout(packId: string, key: string): Promise<Checkout> {
  return call('POST', `/api/market/coin-packs/${encodeURIComponent(packId)}/checkout`, undefined, idem(key))
}

export function listListings(q: { drawingId?: string; cursor?: string | null } = {}): Promise<ListingPage> {
  const params = page(q.cursor, 24)
  if (q.drawingId) params.set('drawing_id', q.drawingId)
  return call('GET', `/api/market/listings?${params.toString()}`)
}

export function createListing(drawingId: string, price: number, key: string): Promise<Listing> {
  return call('POST', '/api/market/listings', { drawing_id: drawingId, price }, idem(key))
}

export function cancelListing(listingId: string): Promise<Listing> {
  return call('DELETE', `/api/market/listings/${encodeURIComponent(listingId)}`)
}

export function purchase(listingId: string, key: string): Promise<Sale> {
  return call('POST', `/api/market/listings/${encodeURIComponent(listingId)}/purchase`, undefined, idem(key))
}

/** Tips the drawers of a round: paid to them when the round ends (ADR 0021). */
export function tip(round: { roomId: string; matchId: string; roundNo: number }, amount: number, key: string): Promise<Tip> {
  return call(
    'POST',
    '/api/market/tips',
    { room_id: round.roomId, match_id: round.matchId, round_no: round.roundNo, amount },
    idem(key),
  )
}

export function listOwnedDrawings(cursor?: string | null): Promise<OwnedPage> {
  const params = page(cursor, 24)
  params.set('owner', 'me')
  return call('GET', `/api/market/drawings?${params.toString()}`)
}

export function getOwnershipHistory(drawingId: string): Promise<OwnershipHistory> {
  return call('GET', `/api/market/drawings/${encodeURIComponent(drawingId)}/ownership-history`)
}

/** "1,250 coins". */
export function coins(n: number): string {
  return `${n.toLocaleString('en-US')} ${Math.abs(n) === 1 ? 'coin' : 'coins'}`
}

/** "$4.99" for a coin pack's price. */
export function money(cents: number, currency: string): string {
  return new Intl.NumberFormat('en-US', { style: 'currency', currency: currency.toUpperCase() }).format(cents / 100)
}
