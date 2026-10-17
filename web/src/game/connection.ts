// One player's connection to a room (design doc, sections 6.4 and 7.4).
//
// Every attempt asks the lobby for a new join ticket (tickets are single-use, ADR 0005) and the
// room's current URL, opens the WebSocket and sends hello {ticket, last_seq}. After a drop it
// reconnects with jittered exponential backoff. Close codes that a retry can't fix (room full,
// no such room, connected elsewhere, a bad first message) end the connection instead.

import { LobbyError, type JoinTicket } from '../lobby.ts'
import {
  CloseCode,
  PING_INTERVAL_MS,
  type ClientMessage,
  type PrivateMessage,
  type RoomEvent,
  type ServerMessage,
  type Welcome,
} from './protocol.ts'

export type ConnectionStatus =
  | { kind: 'connecting' }
  | { kind: 'open' }
  | { kind: 'reconnecting'; attempt: number; delayMs: number }
  | { kind: 'closed'; reason: string }

export type ConnectionHandlers = {
  welcome: (w: Welcome) => void
  event: (e: RoomEvent) => void
  private: (m: PrivateMessage) => void
  status: (s: ConnectionStatus) => void
  rtt?: (ms: number) => void
}

type Deps = {
  join: (roomId: string) => Promise<JoinTicket>
  /** ws://host or wss://host; defaults to the page's host. */
  wsBase?: string
  WebSocket?: typeof WebSocket
  random?: () => number
  now?: () => number
}

export const BACKOFF_BASE_MS = 500
export const BACKOFF_MAX_MS = 8000
/** Give up after this many attempts in a row without hearing from the server (about a minute,
 * which is also how long the room keeps a disconnected player's seat). */
export const MAX_ATTEMPTS = 12

const TERMINAL_CLOSES: Record<number, string> = {
  [CloseCode.BadRequest]: 'The game server rejected this client (bad first message).',
  [CloseCode.RoomFull]: 'This room is full.',
  [CloseCode.NoRoom]: 'This room no longer exists.',
  [CloseCode.Replaced]: 'You joined this room from another tab or device.',
}

export class RoomConnection {
  private readonly roomId: string
  private readonly on: ConnectionHandlers
  private readonly join: Deps['join']
  private readonly wsBase: string
  private readonly WS: typeof WebSocket
  private readonly random: () => number
  private readonly now: () => number

  private ws: WebSocket | null = null
  /** Bumped for every attempt; callbacks from an older attempt are ignored. */
  private generation = 0
  private attempt = 0
  private lastSeq = 0
  /** Whether the current attempt has heard from the server yet. */
  private heard = false
  private stopped = false
  private retryTimer: ReturnType<typeof setTimeout> | null = null
  private pingTimer: ReturnType<typeof setInterval> | null = null

  constructor(roomId: string, handlers: ConnectionHandlers, deps: Deps) {
    this.roomId = roomId
    this.on = handlers
    this.join = deps.join
    this.wsBase =
      deps.wsBase ?? `${window.location.protocol === 'https:' ? 'wss' : 'ws'}://${window.location.host}`
    this.WS = deps.WebSocket ?? WebSocket
    this.random = deps.random ?? Math.random
    this.now = deps.now ?? (() => Math.round(performance.now()))
  }

  start() {
    this.on.status({ kind: 'connecting' })
    void this.connect()
  }

  /** Sends a message if the socket is open. Returns false when it couldn't. */
  send(msg: ClientMessage): boolean {
    if (!this.ws || this.ws.readyState !== this.WS.OPEN) return false
    this.ws.send(JSON.stringify(msg))
    return true
  }

  /** Leaves the room: closes the socket and stops reconnecting. */
  close() {
    this.stopped = true
    this.generation++
    this.clearTimers()
    this.ws?.close(1000, 'leaving')
    this.ws = null
  }

  private async connect() {
    const gen = ++this.generation
    this.heard = false
    let ticket: JoinTicket
    try {
      ticket = await this.join(this.roomId)
    } catch (err) {
      if (gen !== this.generation) return
      if (err instanceof LobbyError && err.code === 'ROOM_NOT_FOUND') {
        return this.finish(TERMINAL_CLOSES[CloseCode.NoRoom])
      }
      if (err instanceof LobbyError && err.code === 'SIGNED_OUT') {
        return this.finish('You are signed out.')
      }
      return this.retry()
    }
    if (gen !== this.generation) return

    const ws = new this.WS(this.wsBase + ticket.ws_url)
    this.ws = ws
    ws.onopen = () => {
      if (gen !== this.generation) return
      ws.send(JSON.stringify({ type: 'hello', data: { ticket: ticket.ticket, last_seq: this.lastSeq } }))
      this.pingTimer = setInterval(() => this.send({ type: 'ping', data: { t: this.now() } }), PING_INTERVAL_MS)
    }
    ws.onmessage = (ev: MessageEvent) => {
      if (gen !== this.generation || typeof ev.data !== 'string') return
      let msg: ServerMessage
      try {
        msg = JSON.parse(ev.data) as ServerMessage
      } catch {
        return
      }
      this.handle(msg)
    }
    ws.onclose = (ev: CloseEvent) => {
      if (gen !== this.generation) return
      this.clearTimers()
      this.ws = null
      const terminal = TERMINAL_CLOSES[ev.code]
      if (terminal) return this.finish(terminal)
      this.retry()
    }
  }

  private handle(msg: ServerMessage) {
    // Hearing from the server means the connection works: start backing off from scratch.
    if (!this.heard) {
      this.heard = true
      this.attempt = 0
      this.on.status({ kind: 'open' })
    }
    switch (msg.type) {
      case 'welcome':
        this.lastSeq = msg.data.seq
        this.on.welcome(msg.data)
        return
      case 'event': {
        const e = msg.data
        if (e.seq <= this.lastSeq) return // already applied
        if (e.seq !== this.lastSeq + 1) {
          // A gap: we missed an event. Reconnect and let the server resend the state.
          this.reconnectNow()
          return
        }
        this.lastSeq = e.seq
        this.on.event(e)
        return
      }
      case 'pong':
        this.on.rtt?.(this.now() - msg.data.t)
        return
      case 'room_moved':
        // The room is on another server now; the lobby knows where.
        this.reconnectNow()
        return
      default:
        this.on.private(msg)
    }
  }

  private reconnectNow() {
    this.generation++
    this.clearTimers()
    this.ws?.close(1000, 'reconnecting')
    this.ws = null
    this.on.status({ kind: 'reconnecting', attempt: 0, delayMs: 0 })
    void this.connect()
  }

  private retry() {
    if (this.stopped) return
    this.attempt++
    if (this.attempt > MAX_ATTEMPTS) {
      return this.finish('Lost the connection to the room.')
    }
    const cap = Math.min(BACKOFF_MAX_MS, BACKOFF_BASE_MS * 2 ** (this.attempt - 1))
    const delayMs = Math.round(cap / 2 + (this.random() * cap) / 2) // jitter: half to full
    this.on.status({ kind: 'reconnecting', attempt: this.attempt, delayMs })
    this.retryTimer = setTimeout(() => void this.connect(), delayMs)
  }

  private finish(reason: string) {
    this.stopped = true
    this.clearTimers()
    this.on.status({ kind: 'closed', reason })
  }

  private clearTimers() {
    if (this.retryTimer) clearTimeout(this.retryTimer)
    if (this.pingTimer) clearInterval(this.pingTimer)
    this.retryTimer = null
    this.pingTimer = null
  }
}
