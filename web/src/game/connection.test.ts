import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LobbyError, type JoinTicket } from '../lobby.ts'
import {
  HANDSHAKE_TIMEOUT_MS,
  MAX_ATTEMPTS,
  PONG_TIMEOUT_MS,
  RoomConnection,
  type ConnectionStatus,
} from './connection.ts'
import { PING_INTERVAL_MS, type Point, type RoomEvent, type Welcome } from './protocol.ts'
import { STROKES_SUBPROTOCOL, encodeStrokeBatch } from './strokeCodec.ts'
import fixtures from './testdata/stroke-frames.json'

const bytes = (h: string) => Uint8Array.from(h.match(/../g) ?? [], (x) => parseInt(x, 16))

class FakeSocket {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3
  static all: FakeSocket[] = []

  readonly url: string
  readonly protocols: string[] | undefined
  /** The subprotocol the server agreed to; '' when none. */
  protocol = ''
  binaryType = 'blob'
  readyState = FakeSocket.CONNECTING
  sent: unknown[] = []
  closedWith: number | null = null
  onopen: (() => void) | null = null
  onmessage: ((ev: { data: string | ArrayBuffer }) => void) | null = null
  onclose: ((ev: { code: number }) => void) | null = null

  constructor(url: string, protocols?: string[]) {
    this.url = url
    this.protocols = protocols
    FakeSocket.all.push(this)
  }

  send(data: string | Uint8Array) {
    this.sent.push(typeof data === 'string' ? JSON.parse(data) : data)
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

  receiveBinary(b: Uint8Array) {
    this.onmessage?.({ data: b.slice().buffer })
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

  it('stays connected after a resume that missed nothing (resumed is the only answer)', async () => {
    await connected(3)
    last().drop(1006)
    await vi.advanceTimersByTimeAsync(500)
    last().open()
    expect(last().sent[0]).toEqual({ type: 'hello', data: { ticket: 'ticket-2', last_seq: 3 } })
    last().receive({ type: 'resumed', data: { seq: 3 } })

    // Past the handshake deadline, answering the liveness pings as a healthy server would.
    for (let waited = 0; waited <= HANDSHAKE_TIMEOUT_MS; waited += PING_INTERVAL_MS) {
      await vi.advanceTimersByTimeAsync(PING_INTERVAL_MS)
      last().receive({ type: 'pong', data: { t: clock } })
    }
    expect(FakeSocket.all).toHaveLength(2)
    expect(last().closedWith).toBeNull()
    expect(statuses.at(-1)).toEqual({ kind: 'open' })
  })

  it('resyncs when resumed says the server is ahead', async () => {
    await connected(3)
    const first = last()
    first.receive(chat(4))
    first.receive({ type: 'resumed', data: { seq: 6 } }) // 5 and 6 never arrived
    expect(first.closedWith).toBe(1000)
    await vi.advanceTimersByTimeAsync(0)
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

  it('stops when the lobby says the room is not live (a viewer, ADR 0017)', async () => {
    join.mockRejectedValueOnce(new LobbyError('ROOM_NOT_LIVE', 409, 'not live'))
    conn.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(statuses.at(-1)).toEqual({ kind: 'closed', reason: "This room isn't live: it can't be watched." })
  })

  it('retries when no relay can take a viewer yet', async () => {
    join.mockRejectedValueOnce(new LobbyError('NO_RELAY', 503, 'busy'))
    conn.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(statuses.at(-1)?.kind).toBe('reconnecting')
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

  describe('binary stroke frames (ADR 0025)', () => {
    const stroke = { stroke_id: 's1', points: [[1, 2, 0]] as Point[], color: '#000000', width: 1, done: false }

    function withWire(wire: 'json' | 'pb') {
      conn.close()
      conn = new RoomConnection(
        'r1',
        { welcome: (w) => welcomes.push(w), event: (e) => events.push(e), private: () => {}, status: () => {} },
        { join, wsBase: 'ws://gw', WebSocket: FakeSocket as unknown as typeof WebSocket, wire },
      )
    }

    it('asks for the subprotocol and sends strokes as binary once the server agrees', async () => {
      withWire('pb')
      await connected()
      expect(last().protocols).toEqual([STROKES_SUBPROTOCOL])
      expect(last().binaryType).toBe('arraybuffer')

      last().protocol = STROKES_SUBPROTOCOL
      conn.send({ type: 'stroke_batch', data: stroke })
      conn.send({ type: 'guess', data: { text: 'cat' } })
      const [, batch, guess] = last().sent
      expect(batch).toEqual(encodeStrokeBatch(stroke))
      expect(guess).toEqual({ type: 'guess', data: { text: 'cat' } })
    })

    it('keeps strokes JSON when the server declines the subprotocol', async () => {
      withWire('pb')
      await connected()
      conn.send({ type: 'stroke_batch', data: stroke })
      expect(last().sent[1]).toEqual({ type: 'stroke_batch', data: stroke })
    })

    it('asks for nothing with the JSON wire', async () => {
      withWire('json')
      await connected()
      expect(last().protocols).toBeUndefined()
    })

    it('delivers a binary stroke event like its JSON form', async () => {
      withWire('pb')
      await connected(fixtures.event.seq - 1) // the event is the next one
      last().receiveBinary(bytes(fixtures.event.hex))
      expect(events).toEqual([
        {
          seq: fixtures.event.seq,
          type: 'stroke_batch',
          data: {
            user_id: fixtures.event.user_id,
            stroke_id: fixtures.event.stroke_id,
            points: fixtures.event.points,
            color: fixtures.event.color,
            width: fixtures.event.width,
            done: fixtures.event.done,
          },
        },
      ])
    })
  })
})
