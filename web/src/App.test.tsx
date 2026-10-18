import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App.tsx'
import { resetAuthForTests } from './auth.ts'

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

describe('App', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  // A signed-in session with one open room; answers by URL.
  function signedIn() {
    fetchMock.mockImplementation((url: string) => {
      switch (url) {
        case '/api/auth/refresh':
          return Promise.resolve(json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 }))
        case '/api/auth/me':
          return Promise.resolve(
            json(200, { id: 'u-1', display_name: 'Ada Lovelace', avatar_url: null, roles: ['player'] }),
          )
        case '/api/lobby/rooms?limit=20':
          return Promise.resolve(
            json(200, {
              rooms: [
                { room_id: 'abc123', host_id: 'u-2', mode: 'classic', players: 2, max_players: 8, phase: 'waiting', created_at: '' },
              ],
              next_cursor: null,
            }),
          )
        case '/api/lobby/leaderboard?limit=20':
          return Promise.resolve(
            json(200, { week: '2026-W42', entries: [{ rank: 1, user_id: 'u-1', name: 'Ada Lovelace', score: 42 }], me: { rank: 1, score: 42 } }),
          )
        case '/api/auth/logout':
          return Promise.resolve(new Response(null, { status: 204 }))
        default:
          return new Promise<Response>(() => {}) // the room's join call: never answers here
      }
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
    window.history.replaceState(null, '', '/')
  })

  it('shows the sign-in link when signed out', async () => {
    fetchMock.mockImplementation(() =>
      Promise.resolve(json(401, { error: { code: 'REFRESH_TOKEN_INVALID' } })),
    )

    render(<App />)

    const link = await screen.findByRole('link', { name: 'Sign in with Google' })
    expect(link.getAttribute('href')).toBe('/api/auth/google/login?return_to=%2F')
    expect(screen.getByRole('heading', { name: 'Doodle Arena' })).toBeTruthy()
  })

  it('shows who is logged in, and signs out', async () => {
    signedIn()

    render(<App />)

    expect(await screen.findByText(/Logged in as Ada Lovelace/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }))
    expect(await screen.findByRole('link', { name: 'Sign in with Google' })).toBeTruthy()
    expect(fetchMock).toHaveBeenLastCalledWith('/api/auth/logout', {
      method: 'POST',
      credentials: 'same-origin',
    })
  })

  it('lists open rooms in the lobby and opens one', async () => {
    signedIn()

    render(<App />)

    expect(await screen.findByText(/Room abc123 · 2\/8 players · waiting/)).toBeTruthy()
    vi.stubGlobal('WebSocket', class {
      static OPEN = 1
      close() {}
    })
    fireEvent.click(screen.getByRole('button', { name: 'Join' }))
    expect(window.location.pathname).toBe('/rooms/abc123')
    expect(await screen.findByRole('heading', { name: 'Room abc123' })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Leave' }))
    expect(window.location.pathname).toBe('/')
  })

  it('opens the weekly leaderboard from the lobby, and goes back', async () => {
    signedIn()

    render(<App />)

    fireEvent.click(await screen.findByRole('link', { name: 'Weekly leaderboard' }))
    expect(window.location.pathname).toBe('/leaderboard')
    expect(await screen.findByText('Ada Lovelace (you)')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Back to lobby' }))
    expect(window.location.pathname).toBe('/')
    expect(await screen.findByRole('heading', { name: 'Lobby' })).toBeTruthy()
  })

  it('shows an error when the server is unreachable', async () => {
    fetchMock.mockRejectedValue(new TypeError('Failed to fetch'))

    render(<App />)

    expect(await screen.findByRole('alert')).toBeTruthy()
  })
  it('signing in from /moderate comes back to it with its next page', async () => {
    window.history.replaceState(null, '', '/moderate?next=/admin/drawings/drawing/')
    fetchMock.mockImplementation(() =>
      Promise.resolve(json(401, { error: { code: 'REFRESH_TOKEN_INVALID' } })),
    )

    render(<App />)

    const link = await screen.findByRole('link', { name: 'Sign in with Google' })
    expect(link.getAttribute('href')).toBe(
      '/api/auth/google/login?return_to=%2Fmoderate%3Fnext%3D%2Fadmin%2Fdrawings%2Fdrawing%2F',
    )
  })

  it('hides the Moderate link from players', async () => {
    signedIn()

    render(<App />)

    expect(await screen.findByText(/Logged in as Ada Lovelace/)).toBeTruthy()
    expect(screen.queryByRole('link', { name: 'Moderate' })).toBeNull()
  })
  it('opens the drawing gallery from the lobby', async () => {
    signedIn()

    render(<App />)
    fireEvent.click(await screen.findByRole('link', { name: 'Drawing gallery' }))

    expect(await screen.findByRole('heading', { name: 'Drawing gallery' })).toBeTruthy()
    expect(window.location.pathname).toBe('/gallery')
  })

  it.each([
    ['Wallet', 'Wallet', '/wallet'],
    ['Market', 'Market', '/market'],
    ['My drawings', 'My drawings', '/my-drawings'],
    ['Developer', 'Developer settings', '/developer'],
  ])('opens %s from the top bar', async (link, heading, path) => {
    signedIn()
    render(<App />)

    fireEvent.click(await screen.findByRole('link', { name: link }))
    expect(await screen.findByRole('heading', { name: heading })).toBeTruthy()
    expect(window.location.pathname).toBe(path)
    expect(screen.getByRole('link', { name: link }).getAttribute('aria-current')).toBe('page')
  })
})
