import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from './auth.ts'
import { Moderate } from './Moderate.tsx'
import { adminNext, signInToAdmin } from './moderation.ts'

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

describe('admin sign-in handoff', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  // Answers the token refresh, the CSRF token, and the sign-in with `signIn`.
  function serve(signIn: () => Response) {
    fetchMock.mockImplementation((url: string, init?: RequestInit) => {
      if (url === '/api/auth/refresh') {
        return Promise.resolve(json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 }))
      }
      if (url === '/admin/sso/' && !init?.method) return Promise.resolve(json(200, { csrf_token: 'c1' }))
      if (url.startsWith('/admin/sso/?') && init?.method === 'POST') return Promise.resolve(signIn())
      return Promise.resolve(json(404, {}))
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

  it('only returns to admin pages', () => {
    expect(adminNext('?next=/admin/drawings/reviewqueue/')).toBe('/admin/drawings/reviewqueue/')
    expect(adminNext('?next=https://evil.example/')).toBe('/admin/')
    expect(adminNext('?next=//evil.example/admin/')).toBe('/admin/')
    expect(adminNext('')).toBe('/admin/')
  })

  it('sends the token once, in the Authorization header, with the CSRF token', async () => {
    serve(() => json(200, { next: '/admin/drawings/reviewqueue/' }))

    const result = await signInToAdmin('/admin/drawings/reviewqueue/')

    expect(result).toEqual({ kind: 'ok', next: '/admin/drawings/reviewqueue/' })
    const posts = fetchMock.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === 'POST')
    const signIns = posts.filter(([url]) => String(url).startsWith('/admin/sso/'))
    expect(signIns).toHaveLength(1)
    const [url, init] = signIns[0] as [string, RequestInit]
    expect(url).toBe('/admin/sso/?next=%2Fadmin%2Fdrawings%2Freviewqueue%2F')
    expect(url).not.toContain('t1')
    const headers = new Headers(init.headers)
    expect(headers.get('Authorization')).toBe('Bearer t1')
    expect(headers.get('X-CSRFToken')).toBe('c1')
  })

  it('tells a missing role from a CSRF failure', async () => {
    serve(() => json(403, { error: { code: 'FORBIDDEN', message: 'role', request_id: '' } }))
    expect(await signInToAdmin('/admin/')).toEqual({ kind: 'forbidden' })

    serve(() => new Response('<h1>Forbidden</h1>', { status: 403, headers: { 'Content-Type': 'text/html' } }))
    expect(await signInToAdmin('/admin/')).toEqual({ kind: 'error' })
  })

  it('opens the admin page it was sent from', async () => {
    window.history.replaceState(null, '', '/moderate?next=/admin/drawings/drawing/')
    serve(() => json(200, { next: '/admin/drawings/drawing/' }))
    const navigate = vi.fn()

    render(<Moderate onBack={() => {}} navigate={navigate} />)

    await waitFor(() => expect(navigate).toHaveBeenCalledWith('/admin/drawings/drawing/'))
    expect(navigate).toHaveBeenCalledTimes(1)
  })

  it('says so when the account has no moderator role', async () => {
    serve(() => json(403, { error: { code: 'FORBIDDEN', message: 'role', request_id: '' } }))
    const navigate = vi.fn()

    render(<Moderate onBack={() => {}} navigate={navigate} />)

    expect((await screen.findByRole('alert')).textContent).toContain("doesn't have the moderator role")
    expect(navigate).not.toHaveBeenCalled()
  })
})
