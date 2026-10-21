import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from './auth.ts'
import { Lobby } from './Lobby.tsx'

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const listing = (over: Record<string, unknown>) => ({
  room_id: 'r1', host_id: 'a', mode: 'classic', players: 2, max_players: 8, phase: 'playing',
  live: false, viewers: 0, created_at: '', ...over,
})

describe('Lobby', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    resetAuthForTests()
    fetchMock = vi.fn((url: string, init?: RequestInit) =>
      Promise.resolve(
        url === '/api/auth/refresh'
          ? json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 })
          : init?.method === 'POST'
            ? json(201, { room_id: 'new', live: true })
            : json(200, { rooms: [listing({ room_id: 'live1', live: true, viewers: 1200 }), listing({ room_id: 'plain' })], next_cursor: null }),
      ),
    )
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('lists live rooms with their viewers, and watches one (ADR 0017)', async () => {
    const onWatch = vi.fn()
    render(<Lobby onEnter={() => {}} onWatch={onWatch} onLeaderboard={() => {}} onGallery={() => {}} />)
    expect(await screen.findByRole('heading', { name: 'Live now' })).toBeTruthy()
    expect(screen.getByText(/Room live1 · 2 playing · 1200 watching/)).toBeTruthy()
    // Watch in "Live now" and in the room list: only for the live room.
    const watch = screen.getAllByRole('button', { name: 'Watch' })
    expect(watch).toHaveLength(2)
    fireEvent.click(watch[0])
    expect(onWatch).toHaveBeenCalledWith('live1')
  })

  it('creates a live room when Live is ticked', async () => {
    const onEnter = vi.fn()
    render(<Lobby onEnter={onEnter} onWatch={() => {}} onLeaderboard={() => {}} onGallery={() => {}} />)
    await screen.findByRole('heading', { name: 'Live now' })
    fireEvent.click(screen.getByLabelText('Live'))
    fireEvent.click(screen.getByRole('button', { name: 'Create a room' }))
    await vi.waitFor(() => expect(onEnter).toHaveBeenCalledWith('new'))
    const create = fetchMock.mock.calls.find(([url, init]) => url === '/api/lobby/rooms' && (init as RequestInit)?.method === 'POST')
    expect(create).toBeDefined()
    expect((create as [string, RequestInit])[1].body).toBe('{"public":true,"live":true}')
  })
})
