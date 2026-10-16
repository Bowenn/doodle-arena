import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fetchMe, loginUrl, logout, refresh, resetAuthForTests } from './auth.ts'

const ME = { id: 'u-1', display_name: 'Ada', avatar_url: null, roles: ['player'] }

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

describe('auth', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    resetAuthForTests()
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('sends only one refresh when callers race', async () => {
    let release!: (r: Response) => void
    fetchMock.mockReturnValueOnce(new Promise<Response>((r) => (release = r)))

    const both = Promise.all([refresh(), refresh(), refresh()])
    release(json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 }))

    expect(await both).toEqual([true, true, true])
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock).toHaveBeenCalledWith('/api/auth/refresh', {
      method: 'POST',
      credentials: 'same-origin',
    })
  })

  it('reports signed out when the refresh is rejected', async () => {
    fetchMock.mockResolvedValueOnce(json(401, { error: { code: 'REFRESH_TOKEN_INVALID' } }))
    expect(await fetchMe()).toBeNull()
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('sends the access token to /me', async () => {
    fetchMock
      .mockResolvedValueOnce(json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 }))
      .mockResolvedValueOnce(json(200, ME))

    expect(await fetchMe()).toEqual(ME)
    expect(fetchMock.mock.calls[1]).toEqual([
      '/api/auth/me',
      { headers: { Authorization: 'Bearer t1' } },
    ])
  })

  it('refreshes once and retries when /me says the token expired', async () => {
    fetchMock
      .mockResolvedValueOnce(json(200, { access_token: 'old', token_type: 'Bearer', expires_in: 900 }))
      .mockResolvedValueOnce(json(401, { error: { code: 'TOKEN_INVALID' } }))
      .mockResolvedValueOnce(json(200, { access_token: 'new', token_type: 'Bearer', expires_in: 900 }))
      .mockResolvedValueOnce(json(200, ME))

    expect(await fetchMe()).toEqual(ME)
    expect(fetchMock.mock.calls[3][1]).toEqual({ headers: { Authorization: 'Bearer new' } })
  })

  it('refreshes again a minute before the token expires', async () => {
    vi.useFakeTimers()
    fetchMock.mockImplementation(() =>
      Promise.resolve(json(200, { access_token: 't', token_type: 'Bearer', expires_in: 900 })),
    )

    await refresh()
    await vi.advanceTimersByTimeAsync(839_000)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1_000)
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('forgets the token on logout', async () => {
    fetchMock
      .mockResolvedValueOnce(json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(json(401, {}))
    await refresh()

    await logout()

    expect(fetchMock.mock.calls[1]).toEqual([
      '/api/auth/logout',
      { method: 'POST', credentials: 'same-origin' },
    ])
    expect(await fetchMe()).toBeNull() // had to refresh again: no token left in memory
  })

  it('builds the login URL with an encoded return path', () => {
    expect(loginUrl('/rooms/1?x=y')).toBe('/api/auth/google/login?return_to=%2Frooms%2F1%3Fx%3Dy')
  })
})
