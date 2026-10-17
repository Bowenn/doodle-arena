import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from './auth.ts'
import { Leaderboard } from './Leaderboard.tsx'
import type { Board } from './lobby.ts'

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

describe('Leaderboard page', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  // Answers the token refresh, then the given boards by URL.
  function serve(boards: Record<string, Board>) {
    fetchMock.mockImplementation((url: string) => {
      if (url === '/api/auth/refresh') {
        return Promise.resolve(json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 }))
      }
      const board = boards[url]
      return Promise.resolve(board ? json(200, board) : json(500, { error: { code: 'INTERNAL', message: 'boom' } }))
    })
  }

  beforeEach(() => {
    resetAuthForTests()
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('shows ranks and names, and highlights your own row', async () => {
    serve({
      '/api/lobby/leaderboard?limit=20': {
        week: '2026-W42',
        entries: [
          { rank: 1, user_id: 'u-1', name: 'Ada', score: 300 },
          { rank: 2, user_id: 'u-2', name: 'Grace', score: 200 },
          { rank: 2, user_id: 'u-3abcdefghij', name: '', score: 200 },
        ],
        me: { rank: 2, score: 200 },
      },
    })

    render(<Leaderboard meId="u-2" onBack={() => {}} />)

    const rows = await screen.findAllByRole('row')
    expect(rows.slice(1).map((r) => r.textContent)).toEqual(['1Ada300', '2Grace (you)200', '2Player u-3abcde200'])
    expect(rows[2].getAttribute('aria-current')).toBe('true')
    expect(rows[1].getAttribute('aria-current')).toBeNull()
    expect(screen.getByText('2026-W42')).toBeTruthy()
    expect(screen.queryByText(/You are #/)).toBeNull()
  })

  it('shows your rank when you are below the list, and steps back a week', async () => {
    serve({
      '/api/lobby/leaderboard?limit=20': {
        week: '2026-W42',
        entries: [{ rank: 1, user_id: 'u-1', name: 'Ada', score: 300 }],
        me: { rank: 7, score: 15 },
      },
      '/api/lobby/leaderboard?week=2026-W41&limit=20': { week: '2026-W41', entries: [], me: null },
    })

    render(<Leaderboard meId="u-9" onBack={() => {}} />)

    expect(await screen.findByText('You are #7 with 15 points.')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Previous week' }))
    expect(await screen.findByText('No matches finished in 2026-W41 yet.')).toBeTruthy()
    expect(screen.getByText("You haven't scored in 2026-W41.")).toBeTruthy()
    expect(screen.queryByRole('table')).toBeNull()
  })

  it('shows the error when the lobby fails', async () => {
    serve({})

    render(<Leaderboard meId="u-1" onBack={() => {}} />)

    expect((await screen.findByRole('alert')).textContent).toBe('boom')
  })
})
