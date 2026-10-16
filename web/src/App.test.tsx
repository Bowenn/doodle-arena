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

  beforeEach(() => {
    resetAuthForTests()
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
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
    fetchMock
      .mockResolvedValueOnce(json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 }))
      .mockResolvedValueOnce(
        json(200, { id: 'u-1', display_name: 'Ada Lovelace', avatar_url: null, roles: ['player'] }),
      )
      .mockResolvedValueOnce(new Response(null, { status: 204 }))

    render(<App />)

    expect(await screen.findByText(/Logged in as Ada Lovelace/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }))
    expect(await screen.findByRole('link', { name: 'Sign in with Google' })).toBeTruthy()
    expect(fetchMock).toHaveBeenLastCalledWith('/api/auth/logout', {
      method: 'POST',
      credentials: 'same-origin',
    })
  })

  it('shows an error when the server is unreachable', async () => {
    fetchMock.mockRejectedValue(new TypeError('Failed to fetch'))

    render(<App />)

    expect(await screen.findByRole('alert')).toBeTruthy()
  })
})
