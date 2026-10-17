import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LobbyError, type JoinTicket } from '../lobby.ts'
import {
  HANDSHAKE_TIMEOUT_MS,
  MAX_ATTEMPTS,
  PONG_TIMEOUT_MS,
  RoomConnection,
  type ConnectionStatus,
} from './connection.ts'
import { PING_INTERVAL_MS, type RoomEvent, type Welcome } from './protocol.ts'

class FakeSocket {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3
  static all: FakeSocket[] = []

  readonly url: string
  readyState = FakeSocket.CONNECTING
  sent: unknown[] = []
  closedWith: number | null = null
  onopen: (() => void) | null = null
  onmessage: ((ev: { data: string }) => void) | null = null
  onclose: ((ev: { code: number }) => void) | null = null

  constructor(url: string) {
    this.url = url
    FakeSocket.all.push(this)
  }

  send(data: string) {
    this.sent.push(JSON.parse(data))
  }

  close(code: number) {
    this.closedWith = code
    this.readyState = FakeSocket.CLOSED
  }

  // Test helpers: what the server does.
  open() {
    this.readyState = FakeSocket.OPEN
    this.onopen?.()
  }

  receive(msg: unknown) {
    this.onmessage?.({ data: JSON.stringify(msg) })
  }

  drop(code: number) {
    this.readyState = FakeSocket.CLOSED
    this.onclose?.({ code })
  }
}

const last = () => FakeSocket.all[FakeSocket.all.length - 1]

function welcome(seq: number): { type: 'welcome'; data: Welcome } {
  return {
    type: 'welcome',
    data: { user_id: 'u1', seq, view: { room_id: 'r1', phase: 'waiting', host_id: 'u1', settings: {}, players: [] } },
  }
}

function chat(seq: number): { type: 'event'; data: RoomEvent } {
  return { type: 'event', data: { seq, type: 'chat', data: { user_id: 'u2', text: `m${seq}` } } }
}

describe('RoomConnection', () => {
  let join: ReturnType<typeof vi.fn<(roomId: string) => Promise<JoinTicket>>>
  let statuses: ConnectionStatus[]
  let events: RoomEvent[]
  let welcomes: Welcome[]
  let rtts: number[]
  let conn: RoomConnection
  let clock: number
  let tickets: number

  beforeEach(() => {
    vi.useFakeTimers()
    FakeSocket.all = []
    statuses = []
    events = []
    welcomes = []
    rtts = []
    clock = 0
    tickets = 0
    join = vi.fn((roomId: string) =>
      Promise.resolve({
        room_id: roomId,
        ws_url: `/ws/gs-1/rooms/${roomId}`,
        ticket: `ticket-${++tickets}`,
        expires_at: '',
      }),
    )
    conn = new RoomConnection(
      'r1',
      {
        welcome: (w) => welcomes.push(w),
        event: (e) => events.push(e),
        private: () => {},
        status: (s) => statuses.push(s),
        rtt: (ms) => rtts.push(ms),
      },
      {
        join,
        wsBase: 'ws://gw',
        WebSocket: FakeSocket as unknown as typeof WebSocket,
        random: () => 1, // no jitter: the full backoff
        now: () => clock,
      },
    )
  })

  afterEach(() => {
    conn.close()
    vi.useRealTimers()
  })

  async function connected(seq = 0) {
    conn.start()
    await vi.advanceTimersByTimeAsync(0)
    last().open()
    last().receive(welcome(seq))
  }

  it('asks the lobby for a ticket and sends hello with it', async () => {
    conn.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(join).toHaveBeenCalledWith('r1')
    expect(last().url).toBe('ws://gw/ws/gs-1/rooms/r1')

    last().open()
    expect(last().sent[0]).toEqual({ type: 'hello', data: { ticket: 'ticket-1', last_seq: 0 } })
    last().receive(welcome(3))
    expect(welcomes).toHaveLength(1)
    expect(statuses.at(-1)).toEqual({ kind: 'open' })
  })

  it('passes events on in seq order and skips ones it already has', async () => {
    await connected(3)
    last().receive(chat(3))
    last().receive(chat(4))
    last().receive(chat(4))
    last().receive(chat(5))
    expect(events.map((e) => e.seq)).toEqual([4, 5])
  })

  it('reconnects with a new ticket and its last seq after a drop', async () => {
    await connected(3)
    last().receive(chat(4))
    last().drop(1006)

    expect(statuses.at(-1)).toEqual({ kind: 'reconnecting', attempt: 1, delayMs: 500 })
    await vi.advanceTimersByTimeAsync(500)
    expect(join).toHaveBeenCalledTimes(2)
    last().open()
    expect(last().sent[0]).toEqual({ type: 'hello', data: { ticket: 'ticket-2', last_seq: 4 } })
  })

  it('backs off exponentially up to the cap, then gives up', async () => {
    await connected()
    const delays: number[] = []
    for (let i = 0; i < MAX_ATTEMPTS; i++) {
      last().drop(1006)
      const s = statuses.at(-1)!
      expect(s.kind).toBe('reconnecting')
      if (s.kind === 'reconnecting') delays.push(s.delayMs)
      await vi.advanceTimersByTimeAsync(s.kind === 'reconnecting' ? s.delayMs : 0)
    }
    expect(delays.slice(0, 6)).toEqual([500, 1000, 2000, 4000, 8000, 8000])

    last().drop(1006)
    expect(statuses.at(-1)).toEqual({ kind: 'closed', reason: 'Lost the connection to the room.' })
  })

  it('starts backing off from scratch once the server answers again', async () => {
    await connected()
    last().drop(1006)
    await vi.advanceTimersByTimeAsync(500)
    last().drop(1006)
    await vi.advanceTimersByTimeAsync(1000)
    last().open()
    last().receive(welcome(0))
    last().drop(1006)
    expect(statuses.at(-1)).toEqual({ kind: 'reconnecting', attempt: 1, delayMs: 500 })
  })

  it.each([
    [4003, 'This room is full.'],
    [4004, 'This room no longer exists.'],
    [4009, 'You joined this room from another tab or device.'],
    [4000, 'The game server rejected this client (bad first message).'],
  ])('stops on close code %i', async (code, reason) => {
    await connected()
    last().drop(code)
    expect(statuses.at(-1)).toEqual({ kind: 'closed', reason })
    await vi.advanceTimersByTimeAsync(60_000)
    expect(join).toHaveBeenCalledTimes(1)
  })

  it.each([4001, 4008, 4010, 1001, 1011, 1013])('retries on close code %i', async (code) => {
    await connected()
    last().drop(code)
    expect(statuses.at(-1)?.kind).toBe('reconnecting')
  })

  it('stops when the lobby says the room is gone', async () => {
    join.mockRejectedValueOnce(new LobbyError('ROOM_NOT_FOUND', 404, 'no such room'))
    conn.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(statuses.at(-1)).toEqual({ kind: 'closed', reason: 'This room no longer exists.' })
  })

  it('retries when the lobby is unavailable', async () => {
    join.mockRejectedValueOnce(new LobbyError('NO_GAME_SERVER', 503, 'busy'))
    conn.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(statuses.at(-1)?.kind).toBe('reconnecting')
    await vi.advanceTimersByTimeAsync(500)
    expect(FakeSocket.all).toHaveLength(1)
  })

  it('pings every 5 seconds and reports the round-trip time', async () => {
    await connected()
    clock = 1000
    await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS)
    expect(last().sent.at(-1)).toEqual({ type: 'ping', data: { t: 1000 } })
    clock = 1042
    last().receive({ type: 'pong', data: { t: 1000 } })
    expect(rtts).toEqual([42])
  })

  it('treats a socket with no answer to a ping as dead and resumes elsewhere', async () => {
    await connected(3)
    last().receive(chat(4))
    const dead = last()
    await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS)
    expect(dead.sent.at(-1)).toMatchObject({ type: 'ping' })
    await vi.advanceTimersByTimeAsync(PONG_TIMEOUT_MS - 1)
    expect(dead.closedWith).toBeNull()

    await vi.advanceTimersByTimeAsync(1)
    expect(dead.closedWith).toBe(1000)
    expect(statuses.at(-1)).toEqual({ kind: 'reconnecting', attempt: 1, delayMs: 500 })
    await vi.advanceTimersByTimeAsync(500)
    expect(join).toHaveBeenCalledTimes(2)
    last().open()
    expect(last().sent[0]).toEqual({ type: 'hello', data: { ticket: 'ticket-2', last_seq: 4 } })

    last().receive(welcome(4))

    // The dead socket's late close and messages change nothing.
    dead.receive(chat(5))
    dead.drop(4010)
    await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS - 1)
    expect(join).toHaveBeenCalledTimes(2)
    expect(events.map((e) => e.seq)).toEqual([4])
  })

  it('keeps a socket that answers every ping', async () => {
    await connected()
    for (let i = 0; i < 20; i++) {
      await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS)
      last().receive({ type: 'pong', data: { t: 0 } })
    }
    expect(FakeSocket.all).toHaveLength(1)
    expect(statuses.at(-1)).toEqual({ kind: 'open' })
  })

  it('counts any server message as an answer to a ping', async () => {
    await connected(3)
    await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS)
    last().receive(chat(4))
    await vi.advanceTimersByTimeAsync(PONG_TIMEOUT_MS)
    expect(last().closedWith).toBeNull()
    expect(FakeSocket.all).toHaveLength(1)
  })

  it('gives up on a socket that never opens', async () => {
    conn.start()
    await vi.advanceTimersByTimeAsync(0)
    const stuck = last()
    await vi.advanceTimersByTimeAsync(HANDSHAKE_TIMEOUT_MS)
    expect(stuck.closedWith).toBe(1000)
    expect(statuses.at(-1)).toEqual({ kind: 'reconnecting', attempt: 1, delayMs: 500 })
    await vi.advanceTimersByTimeAsync(500)
    expect(FakeSocket.all).toHaveLength(2)

    // It opening late changes nothing: no hello on it, no second retry.
    stuck.open()
    expect(stuck.sent).toHaveLength(0)
  })

  it('gives up on a socket that opens but never welcomes', async () => {
    conn.start()
    await vi.advanceTimersByTimeAsync(0)
    last().open()
    await vi.advanceTimersByTimeAsync(HANDSHAKE_TIMEOUT_MS - 1)
    expect(FakeSocket.all).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(last().closedWith).toBe(1000)
    expect(statuses.at(-1)?.kind).toBe('reconnecting')
  })

  it('does not time out a handshake that got its welcome', async () => {
    await connected()
    for (let t = 0; t < HANDSHAKE_TIMEOUT_MS; t += PING_INTERVAL_MS) {
      await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS)
      last().receive({ type: 'pong', data: { t: 0 } })
    }
    expect(last().closedWith).toBeNull()
  })

  it('does not ping before the server has answered hello', async () => {
    conn.start()
    await vi.advanceTimersByTimeAsync(0)
    last().open()
    await vi.advanceTimersByTimeAsync(HANDSHAKE_TIMEOUT_MS - 1)
    expect(last().sent).toEqual([{ type: 'hello', data: { ticket: 'ticket-1', last_seq: 0 } }])
  })

  it('counts dead sockets toward giving up', async () => {
    conn.start()
    for (let i = 0; i < MAX_ATTEMPTS; i++) {
      await vi.advanceTimersByTimeAsync(0)
      await vi.advanceTimersByTimeAsync(HANDSHAKE_TIMEOUT_MS)
      const s = statuses.at(-1)!
      expect(s.kind).toBe('reconnecting')
      if (s.kind === 'reconnecting') await vi.advanceTimersByTimeAsync(s.delayMs)
    }
    await vi.advanceTimersByTimeAsync(HANDSHAKE_TIMEOUT_MS)
    expect(statuses.at(-1)).toEqual({ kind: 'closed', reason: 'Lost the connection to the room.' })
  })

  it('resyncs through a new connection when an event is missing', async () => {
    await connected(3)
    const first = last()
    first.receive(chat(5)) // 4 is missing
    expect(events).toHaveLength(0)
    expect(first.closedWith).toBe(1000)
    await vi.advanceTimersByTimeAsync(0)
    expect(FakeSocket.all).toHaveLength(2)
    last().open()
    expect(last().sent[0]).toEqual({ type: 'hello', data: { ticket: 'ticket-2', last_seq: 3 } })
  })

  it('reconnects through the lobby after a short backoff on room_moved', async () => {
    await connected(3)
    const old = last()
    old.receive({ type: 'room_moved', data: { reason: 'failover' } })
    expect(old.closedWith).toBe(1000)
    expect(statuses.at(-1)).toEqual({ kind: 'reconnecting', attempt: 1, delayMs: 500 })
    await vi.advanceTimersByTimeAsync(0)
    expect(join).toHaveBeenCalledTimes(1)

    old.drop(4010) // the server's close right after room_moved
    await vi.advanceTimersByTimeAsync(500)
    expect(join).toHaveBeenCalledTimes(2)
    last().open()
    expect(last().sent[0]).toEqual({ type: 'hello', data: { ticket: 'ticket-2', last_seq: 3 } })
    last().receive(welcome(3))
    await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS - 1)
    expect(join).toHaveBeenCalledTimes(2)
  })

  it('ignores the old socket after close and does not reconnect', async () => {
    await connected()
    const sock = last()
    conn.close()
    expect(sock.closedWith).toBe(1000)
    sock.drop(1000)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(join).toHaveBeenCalledTimes(1)
  })

  it('sends only while the socket is open', async () => {
    expect(conn.send({ type: 'guess', data: { text: 'x' } })).toBe(false)
    await connected()
    expect(conn.send({ type: 'guess', data: { text: 'x' } })).toBe(true)
    expect(last().sent.at(-1)).toEqual({ type: 'guess', data: { text: 'x' } })
  })
})
