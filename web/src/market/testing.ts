// Shared fetch mocking for the market pages' tests: answers by method and URL.

import { vi } from 'vitest'
import type { Drawing } from '../gallery.ts'
import type { Listing } from '../money.ts'

export function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

export type Route = (init: RequestInit) => Response | Promise<Response>

/** Stubs fetch: routes are "METHOD url"; a refresh always succeeds; anything else is a 500. */
export function mockApi(routes: Record<string, Route>) {
  const fetchMock = vi.fn((url: string, init: RequestInit = {}) => {
    if (url === '/api/auth/refresh') {
      return Promise.resolve(json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 }))
    }
    const route = routes[`${init.method ?? 'GET'} ${url}`]
    if (!route) return Promise.resolve(json(500, { error: { code: 'INTERNAL', message: `unexpected ${url}` } }))
    try {
      return Promise.resolve(route(init))
    } catch (e) {
      return Promise.reject(e)
    }
  })
  vi.stubGlobal('fetch', fetchMock)
  return {
    fetchMock,
    /** The Idempotency-Key of each call to "METHOD url", in order. */
    keys(route: string): (string | null)[] {
      return fetchMock.mock.calls
        .filter(([u, init]) => `${init?.method ?? 'GET'} ${u}` === route)
        .map(([, init]) => new Headers(init?.headers).get('Idempotency-Key'))
    },
  }
}

export function drawing(id: string, word: string): Drawing {
  return {
    id,
    match_id: 'm1',
    round_no: 1,
    word,
    author_ids: ['u_ada'],
    initial_owner_id: 'u_ada',
    status: 'approved',
    ended_at: '',
    created_at: '',
    has_replay: false,
    image_url: `http://s3/${word}.png`,
    thumb_url: `http://s3/${word}-thumb.png`,
  }
}

export function listing(n: number, over: Partial<Listing> = {}): Listing {
  return {
    listing_id: `l${n}`,
    drawing_id: `00000000-0000-7000-8000-00000000000${n}`,
    seller_id: 'u_ada',
    price: 60,
    status: 'ACTIVE',
    buyer_id: null,
    sale_id: null,
    created_at: '2026-10-18T10:00:00Z',
    closed_at: null,
    ...over,
  }
}

export function sale(l: Listing, buyer = 'u_me') {
  return {
    sale_id: 's1',
    listing_id: l.listing_id,
    drawing_id: l.drawing_id,
    seller_id: l.seller_id,
    buyer_id: buyer,
    price: l.price,
    royalties: [],
    completed_at: '2026-10-18T10:01:00Z',
  }
}
