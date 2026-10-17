import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from './auth.ts'
import { createRoom, joinRoom, LobbyError } from './lobby.ts'

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

const token = () => json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 })

describe('lobby client', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    resetAuthForTests()
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('creates a room with a JSON body', async () => {
    fetchMock.mockResolvedValueOnce(token()).mockResolvedValueOnce(json(201, { room_id: 'abc' }))

    expect(await createRoom(false)).toEqual({ room_id: 'abc' })
    const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit]
    expect(url).toBe('/api/lobby/rooms')
    expect(init.method).toBe('POST')
    expect(init.body).toBe('{"public":false}')
  })

  it('turns the error shape into a LobbyError with its code', async () => {
    fetchMock
      .mockResolvedValueOnce(token())
      .mockResolvedValueOnce(json(404, { error: { code: 'ROOM_NOT_FOUND', message: 'no such room' } }))

    const err = await joinRoom('nope').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(LobbyError)
    expect((err as LobbyError).code).toBe('ROOM_NOT_FOUND')
    expect((err as LobbyError).status).toBe(404)
  })

  it('reports SIGNED_OUT when there is no session', async () => {
    fetchMock.mockResolvedValueOnce(json(401, {}))
    const err = await joinRoom('abc').catch((e: unknown) => e)
    expect((err as LobbyError).code).toBe('SIGNED_OUT')
  })
})
