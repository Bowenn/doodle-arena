// Lobby REST client (design doc, section 6.3).

import { call } from './api.ts'

export type RoomListing = {
  room_id: string
  host_id: string
  mode: string
  players: number
  max_players: number
  phase: string
  created_at: string
}

export type Room = {
  room_id: string
  host_id: string
  mode: string
  public: boolean
  created_at: string
}

/** A join ticket: single-use, so every (re)connect asks for a new one. */
export type JoinTicket = {
  room_id: string
  ws_url: string
  ticket: string
  expires_at: string
}

// Lobby calls fail with the shared ApiError; the old name stays for the lobby's callers.
export { ApiError as LobbyError } from './api.ts'

export function listRooms(): Promise<{ rooms: RoomListing[]; next_cursor: string | null }> {
  return call('GET', '/api/lobby/rooms?limit=20')
}

export function createRoom(isPublic: boolean): Promise<Room> {
  return call('POST', '/api/lobby/rooms', { public: isPublic })
}

export function joinRoom(roomId: string): Promise<JoinTicket> {
  return call('POST', `/api/lobby/rooms/${encodeURIComponent(roomId)}/join`, {})
}

export function quickPlay(): Promise<JoinTicket> {
  return call('POST', '/api/lobby/quickplay', {})
}

export type LeaderboardEntry = {
  rank: number
  user_id: string
  /** Empty if the lobby never saw the player join. */
  name: string
  score: number
}

/** One ISO week's leaderboard (weeks run Monday to Sunday, UTC). me is null if you scored nothing. */
export type Board = {
  week: string
  entries: LeaderboardEntry[]
  me: { rank: number; score: number } | null
}

/** week is an ISO week such as 2026-W42; without it, the server picks the current week. */
export function getLeaderboard(week?: string, limit = 20): Promise<Board> {
  const q = new URLSearchParams()
  if (week) q.set('week', week)
  q.set('limit', String(limit))
  return call('GET', `/api/lobby/leaderboard?${q.toString()}`)
}
