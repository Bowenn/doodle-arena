// Extra players for a live match, without Google sign-in. Each seat runs the web app's own
// RoomConnection, roomReducer and StrokeBatcher in Node 24 (native TypeScript, global
// WebSocket) through the gateway, with an access token minted by mint_tokens.py. This is how
// the M2 match with five web clients was played: a signed-in browser hosts, and these fill
// the other seats. Run it from the repo root against `make up`:
//
//   node scripts/drills/web-seats.mts <room_id>        join the browser's room (it starts it)
//   node scripts/drills/web-seats.mts                  bots only: seat 1 creates, hosts, starts
//
// Options: --seats <n> (default 4), --guess-after <seconds> (default 25). In a worktree, set
// DRILL_JWT_KEY to the main checkout's secrets/jwt-private.pem (see mint_tokens.py).
//
// The first drawer picks the first word option. Drawers draw a circle each, and the first
// drawer of a pair also draws a scribble and undoes it. Guessers wait, send one wrong guess,
// then the word: a drawer seat knows it, and when only the browser draws, write
// "<round> <word>" to word.txt in the directory the script prints at start (under the system
// temp directory). Each drawer seat writes the word to current-word.txt there, so whoever
// plays the browser can guess it too. The script exits 0 when every seat saw the same results
// and the same events from match_started on, and 1 otherwise (or when a seat's connection
// ends for good, or after 10 minutes without a match end).

import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { parseArgs } from 'node:util'
import { fileURLToPath } from 'node:url'

import { RoomConnection, type ConnectionStatus } from '../../web/src/game/connection.ts'
import type { PrivateMessage, Result, RoomEvent, Welcome } from '../../web/src/game/protocol.ts'
import { choosingLabel, drawingLabel, roomReducer, type RoomState } from '../../web/src/game/roomState.ts'
import { StrokeBatcher } from '../../web/src/game/strokes.ts'
import { LobbyError, type JoinTicket } from '../../web/src/lobby.ts'

const HERE = dirname(fileURLToPath(import.meta.url))
const ROOT = resolve(HERE, '../..')
// GATEWAY=https://icanfly.click plays in AWS (with DRILL_JWT_PEM, see mint_tokens.py).
const GW = process.env.GATEWAY ?? 'http://localhost:8080'
const WS = GW.replace(/^http/, 'ws')
const NAMES = ['Ada', 'Bob', 'Cyd', 'Dee', 'Eve', 'Fay', 'Gus']
const COLORS = ['#e53935', '#1e88e5', '#43a047', '#8e24aa', '#fb8c00', '#6d4c41', '#00897b']
const GIVE_UP_MS = 10 * 60_000
const WORDS_DIR = join(tmpdir(), 'doodle-web-seats')

const { values, positionals } = parseArgs({
  allowPositionals: true,
  options: { seats: { type: 'string', default: '4' }, 'guess-after': { type: 'string', default: '25' } },
})
const seatCount = Number(values.seats)
const guessAfterMs = Number(values['guess-after']) * 1000
if (!(seatCount >= 1 && seatCount <= NAMES.length) || !(guessAfterMs >= 0) || positionals.length > 1) {
  console.error('usage: node scripts/drills/web-seats.mts [room_id] [--seats 1-7] [--guess-after seconds]')
  process.exit(2)
}

type Player = { id: string; name: string; token: string }

// The tokens stay in this process's memory.
const players = JSON.parse(
  execFileSync(
    'uv',
    ['run', '-q', '--project', 'services/auth', 'python', 'scripts/drills/mint_tokens.py', ...NAMES.slice(0, seatCount)],
    { cwd: ROOT, encoding: 'utf8' },
  ),
) as Player[]

const t0 = Date.now()
function log(who: string, msg: string) {
  console.log(`${((Date.now() - t0) / 1000).toFixed(1).padStart(6)} ${who.padEnd(3)} ${msg}`)
}
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

async function lobby<T>(method: string, path: string, token: string): Promise<T> {
  const resp = await fetch(GW + path, {
    method,
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: method === 'POST' ? JSON.stringify(path.endsWith('/rooms') ? { public: false } : {}) : undefined,
  })
  const body = (await resp.json().catch(() => null)) as { error?: { code?: string; message?: string } } | null
  if (!resp.ok) {
    throw new LobbyError(body?.error?.code ?? `HTTP_${resp.status}`, resp.status, body?.error?.message ?? `${path} failed`)
  }
  return body as T
}

// The word of the current round, when a seat draws it (or word.txt says it).
let roundWord: { round: number; word: string } | null = null
const WORD_FILE = join(WORDS_DIR, 'word.txt')

function knownWord(round: number): string | null {
  if (roundWord?.round === round) return roundWord.word
  if (existsSync(WORD_FILE)) {
    const [r, w] = readFileSync(WORD_FILE, 'utf8').trim().split(/\s+/)
    if (Number(r) === round && w) return w
  }
  return null
}

const ended = new Map<string, { results: Result[]; seqs: number[] }>()

class Seat {
  readonly player: Player
  readonly idx: number
  readonly conn: RoomConnection
  state: RoomState | null = null
  /** The seqs of every event from match_started on, to compare the order across seats. */
  readonly seqs: number[] = []
  /** Resolves on the first welcome. */
  readonly welcomed: Promise<void>
  private markWelcomed: () => void = () => {}

  constructor(player: Player, idx: number, roomId: string) {
    this.player = player
    this.idx = idx
    this.welcomed = new Promise((r) => (this.markWelcomed = r))
    this.conn = new RoomConnection(
      roomId,
      {
        welcome: (w: Welcome) => {
          this.state = roomReducer(this.state, { type: 'welcome', welcome: w })
          log(this.player.name, `welcome seq=${w.seq} phase=${w.view.phase}`)
          this.markWelcomed()
        },
        event: (e: RoomEvent) => {
          this.state = roomReducer(this.state, { type: 'event', event: e })
          if (this.state?.matchId) this.seqs.push(e.seq)
          this.onEvent(e)
        },
        private: (m: PrivateMessage) => {
          this.state = roomReducer(this.state, { type: 'private', msg: m })
          void this.onPrivate(m)
        },
        status: (s: ConnectionStatus) => {
          log(this.player.name, `status ${s.kind}${s.kind === 'closed' ? ': ' + s.reason : ''}`)
          // A closed connection doesn't retry any more (room full, no such room, signed out).
          if (s.kind === 'closed' && !ended.has(this.player.name)) process.exit(1)
        },
      },
      { join: (id: string) => lobby<JoinTicket>('POST', `/api/lobby/rooms/${encodeURIComponent(id)}/join`, this.player.token), wsBase: WS },
    )
  }

  private get s(): RoomState {
    if (!this.state) throw new Error('no state before welcome')
    return this.state
  }

  private drawing(round: number): boolean {
    return this.state?.roundNo === round && this.state.phase === 'drawing'
  }

  // Only seat 0 logs shared events, so the output reads as one timeline.
  private onEvent(e: RoomEvent) {
    if (this.idx === 0) logShared(e, this.s)
    if (e.type === 'round_started' && !e.data.drawer_ids.includes(this.player.id)) void this.guess(e.data.round_no)
    if (e.type === 'match_ended') {
      ended.set(this.player.name, { results: e.data.results, seqs: this.seqs })
      if (ended.size === seats.length) finish()
    }
  }

  private async onPrivate(m: PrivateMessage) {
    const name = this.player.name
    if (m.type === 'word_options') {
      log(name, `word_options ${m.data.words.join(', ')}; picking ${m.data.words[0]}`)
      await sleep(2000)
      this.conn.send({ type: 'choose_word', data: { index: 0 } })
    } else if (m.type === 'your_word') {
      const { roundNo, drawerIds } = this.s
      log(name, `your_word ${m.data.word} (drawers ${drawerIds.join(', ')})`)
      roundWord = { round: roundNo, word: m.data.word }
      writeFileSync(join(WORDS_DIR, 'current-word.txt'), `${roundNo} ${m.data.word}\n`)
      await this.draw(roundNo, drawerIds.length === 2 && drawerIds[0] === this.player.id)
    } else if (m.type === 'guess_result') {
      log(name, `guess_result correct=${m.data.correct}`)
    } else {
      log(name, `${m.type} ${JSON.stringify(m.data)}`)
    }
  }

  private async draw(round: number, alsoUndo: boolean) {
    const batcher = new StrokeBatcher((batch) => this.conn.send({ type: 'stroke_batch', data: batch }), { intervalMs: 50 })
    await sleep(1000 + this.idx * 700)
    // A circle in this seat's part of the canvas (coordinates are 0..10000).
    const cx = 2500 + (this.idx % 2) * 5000
    const cy = 3000 + (Math.floor(this.idx / 2) % 2) * 4000
    batcher.begin(cx + 1500, cy, COLORS[this.idx], 80)
    for (let i = 1; i <= 40 && this.drawing(round); i++) {
      const a = (i / 40) * 2 * Math.PI
      batcher.add(Math.round(cx + 1500 * Math.cos(a)), Math.round(cy + 1500 * Math.sin(a)))
      await sleep(25)
    }
    batcher.end()
    if (!alsoUndo || !this.drawing(round)) return
    await sleep(800)
    const id = batcher.begin(cx - 1000, cy - 1000, '#000000', 120)
    for (let i = 1; i <= 20; i++) {
      batcher.add(cx - 1000 + i * 100, cy - 1000 + (i % 2) * 2000)
      await sleep(25)
    }
    batcher.end()
    log(this.player.name, `drew scribble ${id}, undoing it in 4 s`)
    await sleep(4000)
    if (this.drawing(round)) this.conn.send({ type: 'undo', data: { stroke_id: id } })
  }

  private async guess(round: number) {
    await sleep(guessAfterMs + this.idx * 1500)
    if (!this.drawing(round)) return
    this.conn.send({ type: 'guess', data: { text: 'potato' } })
    while (this.drawing(round)) {
      await sleep(500)
      const word = knownWord(round)
      if (word && this.drawing(round)) {
        log(this.player.name, `guessing ${word}`)
        this.conn.send({ type: 'guess', data: { text: word } })
        return
      }
    }
  }
}

function logShared(e: RoomEvent, s: RoomState) {
  const line = (msg: string) => log('---', `#${e.seq} ${msg}`)
  switch (e.type) {
    case 'choosing':
      return line(`choosing round ${e.data.round_no}: "${choosingLabel(s)}"`)
    case 'round_started':
      return line(`round ${e.data.round_no} drawing: "${drawingLabel(s)}", ${e.data.word_length} letters`)
    case 'stroke_batch':
      return e.data.done ? line(`stroke ${e.data.stroke_id} by ${e.data.user_id}`) : undefined
    case 'round_ended': {
      const scores = e.data.scores.map((x) => `${x.name}:${x.score}`).join(' ')
      return line(`round ${e.data.round_no} ended, word=${e.data.word} points=${JSON.stringify(e.data.round_points)} scores=${scores}`)
    }
    case 'match_ended':
      return line(`match ended: ${e.data.results.map((r) => `${r.placement}.${r.name}:${r.score}`).join(' ')}`)
    default:
      return line(`${e.type} ${JSON.stringify(e.data)}`)
  }
}

function finish() {
  const all = [...ended.values()]
  const same = (pick: (x: (typeof all)[number]) => unknown) =>
    all.every((x) => JSON.stringify(pick(x)) === JSON.stringify(pick(all[0])))
  const ok = same((x) => x.results) && same((x) => x.seqs)
  log('---', `all ${all.length} seats saw match_ended; same results and event order: ${ok}`)
  for (const seat of seats) seat.conn.close()
  setTimeout(() => process.exit(ok ? 0 : 1), 500)
}

setTimeout(() => {
  log('---', `no match_ended within ${GIVE_UP_MS / 60_000} minutes`)
  process.exit(1)
}, GIVE_UP_MS)

mkdirSync(WORDS_DIR, { recursive: true })
for (const f of ['word.txt', 'current-word.txt']) writeFileSync(join(WORDS_DIR, f), '')
log('---', `word files in ${WORDS_DIR}`)

let roomId = positionals[0]
const botsOnly = !roomId
if (botsOnly) {
  roomId = (await lobby<{ room_id: string }>('POST', '/api/lobby/rooms', players[0].token)).room_id
  log('---', `created room ${roomId}, hosted by ${players[0].name}`)
}
const seats = players.map((p, i) => new Seat(p, i, roomId))
// Seats join one after another, so the join order (and the draw order) is the seat order.
for (const seat of seats) {
  seat.conn.start()
  await seat.welcomed
}
if (botsOnly) {
  await sleep(1000)
  seats[0].conn.send({ type: 'start', data: {} })
}
