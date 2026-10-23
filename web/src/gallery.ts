// Drawings REST client (design doc, section 6.3): the gallery, one drawing, its replay.

import { call } from './api.ts'

export type DrawingStatus = 'processing' | 'review' | 'approved' | 'rejected' | 'failed'

export type Drawing = {
  id: string
  match_id: string
  round_no: number
  word: string
  author_ids: string[]
  initial_owner_id: string
  status: DrawingStatus
  ended_at: string
  created_at: string
  has_replay: boolean
  /** Presigned URLs straight to storage, valid 15 minutes; null until the images exist. */
  image_url: string | null
  thumb_url: string | null
}

export type DrawingPage = { drawings: Drawing[]; next_cursor: string | null }

/** [x, y, t_ms]: x and y normalized to 0..10000, t_ms since the stroke began (client clock). */
export type ReplayPoint = [number, number, number]

export type ReplayStroke = {
  user_id: string
  color: string
  width: number
  undone: boolean
  /** When the game server got the stroke's last batch, in ms after the round started. */
  server_received_ms: number
  points: ReplayPoint[]
}

export type Replay = {
  drawing_id: string
  schema_version: number
  canvas_width: number
  canvas_height: number
  round_started_at: string | null
  strokes: ReplayStroke[]
}

export type DrawingQuery = { authorId?: string; matchId?: string; cursor?: string | null; limit?: number }

/** Without a filter, every approved drawing; with one, also the caller's own in any state. */
export function listDrawings(q: DrawingQuery = {}): Promise<DrawingPage> {
  const params = new URLSearchParams()
  if (q.authorId) params.set('author_id', q.authorId)
  if (q.matchId) params.set('match_id', q.matchId)
  if (q.cursor) params.set('cursor', q.cursor)
  params.set('limit', String(q.limit ?? 24))
  return call('GET', `/api/assets/drawings?${params.toString()}`)
}

/** Search (ADR 0023): approved drawings ranked by Elasticsearch, with their active listing. */
export type SearchSort = 'relevance' | 'newest' | 'price_asc' | 'price_desc'

export type SearchQuery = {
  q?: string
  listed?: boolean
  minPrice?: number
  maxPrice?: number
  authorId?: string
  sort?: SearchSort
  cursor?: string | null
}

export type SearchHit = Drawing & { listing: { listing_id: string; price: number; seller_id: string } | null }

export type PriceFacet = { key: string; from: number | null; to: number | null; count: number }

export type SearchPage = {
  drawings: SearchHit[]
  total: number
  facets: { price: PriceFacet[] }
  next_cursor: string | null
}

export function searchDrawings(s: SearchQuery): Promise<SearchPage> {
  const params = new URLSearchParams()
  if (s.q) params.set('q', s.q)
  if (s.listed) params.set('listed', 'true')
  if (s.minPrice !== undefined) params.set('min_price', String(s.minPrice))
  if (s.maxPrice !== undefined) params.set('max_price', String(s.maxPrice))
  if (s.authorId) params.set('author_id', s.authorId)
  if (s.sort) params.set('sort', s.sort)
  if (s.cursor) params.set('cursor', s.cursor)
  params.set('limit', '24')
  return call('GET', `/api/assets/search?${params.toString()}`)
}

/** Several drawings by id (at most 50), in the order asked; ones the caller can't see are left out. */
export async function getDrawingsByIds(ids: string[]): Promise<Drawing[]> {
  if (ids.length === 0) return []
  const page = await call<DrawingPage>('GET', `/api/assets/drawings?ids=${ids.map(encodeURIComponent).join(',')}`)
  return page.drawings
}

export function getDrawing(id: string): Promise<Drawing> {
  return call('GET', `/api/assets/drawings/${encodeURIComponent(id)}`)
}

export function getReplay(id: string): Promise<Replay> {
  return call('GET', `/api/assets/drawings/${encodeURIComponent(id)}/replay`)
}
