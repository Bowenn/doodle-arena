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

  it('shows an error when the server is unreachable', async () => {
    fetchMock.mockRejectedValue(new TypeError('Failed to fetch'))

    render(<App />)

    expect(await screen.findByRole('alert')).toBeTruthy()
  })
})
