// Many small rooms (task 5.3): each virtual user plays one room of four players over four
// WebSockets, through the gateway like the web app does (design doc 6.3, 6.4, 7.2 to 7.4).
//
//   scripts/loadtest.sh            the full ramp (10, 40, then 80 rooms)
//   scripts/loadtest.sh smoke      2 rooms for about a minute (CI's stack job)
//
// The script needs the players' access tokens in TOKENS (a JSON array, four per room, in
// order), which scripts/loadtest.sh mints and passes in the environment: never in a file.
//
// What a room does: the host creates a private room and connects first (whoever connects first
// becomes the room's host, design doc 7.3), then the other three join, and the host starts a
// match. With four players every round has two drawers (task 2.6a): the first picks a word,
// both send a stroke batch every 50 ms. The virtual user holds all four sockets, so it knows
// the word: after 10 to 30 s each guesser makes two wrong guesses, then the right one, and the
// round ends early, as in a real game. After a match the host starts the next one.
//
// A dropped socket does what the web app does (design doc 7.4): a new ticket from the lobby,
// then hello with the last seq it saw, with backoff from 0.5 s doubling to 8 s for about a
// minute. So a game server killed under load (scripts/drills/failover.py) shows up as resumes,
// not as failed rooms.
//
// Client-side metrics, in ms (sent to Grafana with --out opentelemetry, as k6_<name>):
//   stroke_fanout     a drawer's stroke batch until each other player receives it. The send time
//                     is in the stroke_id, so no protocol change is needed. All four sockets live
//                     in one process, so both ends use the same clock.
//   guess_result      a guess until its guess_result
//   ws_rtt            ping until pong: the network round trip alone
//   join              POST /join until the ticket arrives; welcome: connect until welcome
//   resume            a dropped socket until it has resumed (resumed or welcome)
//   reconnects        drops that were resumed, tagged with the close code
//   room_errors       rooms that failed, tagged with the step (also logged with the cause)
//   matches_completed, rounds_completed
import http from 'k6/http'
import exec from 'k6/execution'
import { sleep } from 'k6'
import { WebSocket } from 'k6/websockets'
import { setInterval, setTimeout, clearInterval, clearTimeout } from 'k6/timers'
import { Counter, Trend } from 'k6/metrics'

const GATEWAY = __ENV.GATEWAY || 'http://localhost:8080'
const WS_BASE = GATEWAY.replace(/^http/, 'ws')
const TOKENS = JSON.parse(__ENV.TOKENS || '[]')
const PLAYERS = 4
const SMOKE = __ENV.PROFILE === 'smoke'
// How long one room stays open: a few matches, then the virtual user leaves and starts over.
const ROOM_SECONDS = Number(__ENV.ROOM_SECONDS || (SMOKE ? 50 : 420))
// A dropped socket retries for about as long as the web app does.
const RESUME_GIVE_UP_MS = 60000

const strokeFanout = new Trend('stroke_fanout', true)
const guessResult = new Trend('guess_result', true)
const wsRtt = new Trend('ws_rtt', true)
const joinTime = new Trend('join', true)
const welcomeTime = new Trend('welcome', true)
const resumeTime = new Trend('resume', true)
const reconnects = new Counter('reconnects')
const matches = new Counter('matches_completed')
const rounds = new Counter('rounds_completed')
const roomErrors = new Counter('room_errors')

// Counts an error and says what it was: the summary only has the total.
function roomError(step, detail) {
  roomErrors.add(1, { step })
  console.warn(`room error (VU ${exec.vu.idInTest}): ${step}${detail ? ': ' + detail : ''}`)
}

export const options = {
  scenarios: {
    rooms: SMOKE
      ? { executor: 'per-vu-iterations', vus: 2, iterations: 1, maxDuration: '2m' }
      : {
          executor: 'ramping-vus',
          startVUs: 0,
          // Rooms arrive about one a second at most: each costs nine gateway requests (create,
          // four joins, four upgrades), and the gateway allows 20 a second per address.
          stages: [
            { duration: '30s', target: 10 },
            { duration: '2m', target: 10 },
            { duration: '1m', target: 40 },
            { duration: '3m', target: 40 },
            { duration: '1m', target: 80 },
            { duration: '4m', target: 80 },
            { duration: '30s', target: 0 },
          ],
          gracefulRampDown: '20s',
          gracefulStop: '20s',
        },
  },
  thresholds: {
    // A fifth of a second for a stroke to reach the other players, including the client's own
    // network path (ws_rtt says how much of it that is).
    stroke_fanout: ['p(95)<250'],
    room_errors: ['count<5'],
    http_req_failed: ['rate<0.01'],
  },
}

function api(method, path, token, body) {
  return http.request(method, GATEWAY + path, body === undefined ? null : JSON.stringify(body), {
    headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' },
    tags: { name: path.replace(/rooms\/[a-z0-9]+/, 'rooms/{id}') },
    timeout: '10s',
  })
}

const send = (s, type, data) => s.ws && s.ws.readyState === 1 && s.ws.send(JSON.stringify({ type, data }))

// A virtual user whose room failed waits before the next one, so a broken stack sees a trickle
// of retries instead of a flood (2 s doubling to 30 s, with jitter).
let failures = 0

export default function () {
  if (failures > 0) sleep(Math.min(30, 2 ** failures) * (0.5 + Math.random() / 2))

  // Four tokens per virtual user, in order: the first player is the host.
  const base = ((exec.vu.idInTest - 1) * PLAYERS) % TOKENS.length
  const players = TOKENS.slice(base, base + PLAYERS)
  if (players.length < PLAYERS) {
    throw new Error(`TOKENS has ${TOKENS.length} tokens; scripts/loadtest.sh mints four per room`)
  }

  const created = api('POST', '/api/lobby/rooms', players[0].token, { public: false })
  if (created.status >= 300) {
    roomError('create', String(created.status))
    failures++
    return
  }
  const room = created.json()

  const state = { word: '', drawers: [], round: 0, seq: 0, host: players[0].id, closing: false }
  const sockets = players.map((player, index) => ({
    player, index, ws: null, lastSeq: 0, connectedOnce: false, replaying: false, guessSent: 0,
    knowsWord: false, guessedRound: 0, droppedAt: 0, attempt: 0, resumeCode: 0, pinger: null,
  }))
  const timers = []
  let welcomed = 0
  let strokeSeq = 0

  const later = (ms, fn) => timers.push({ timeout: setTimeout(fn, ms) })
  const every = (ms, fn) => {
    const id = setInterval(fn, ms)
    timers.push({ interval: id })
    return id
  }

  function closeAll() {
    state.closing = true
    for (const t of timers) {
      if (t.interval !== undefined) clearInterval(t.interval)
      else clearTimeout(t.timeout)
    }
    for (const s of sockets) {
      if (s.pinger !== null) clearInterval(s.pinger)
      if (s.ws) s.ws.close()
    }
  }

  function startMatch() {
    const host = sockets.find((x) => x.player.id === state.host)
    if (host && !state.closing) send(host, 'start', {})
  }

  function startDrawing(roundNo) {
    // Both drawers send a stroke batch every 50 ms until the round ends.
    for (const s of sockets.filter((x) => state.drawers.includes(x.player.id))) {
      let points = 0
      let strokeStarted = 0
      let strokeId = ''
      const id = every(50, () => {
        if (state.round !== roundNo || state.closing) return clearInterval(id)
        const now = Date.now()
        if (points === 0) {
          strokeSeq++
          // The stroke's start time rides in its id, so receivers can time the fan-out.
          strokeId = `t${now}_${exec.vu.idInTest}_${strokeSeq}`
          strokeStarted = now
        }
        const batch = []
        for (let i = 0; i < 8; i++) {
          batch.push([Math.floor(Math.random() * 10000), Math.floor(Math.random() * 10000), now - strokeStarted])
        }
        points += batch.length
        const done = points >= 160
        send(s, 'stroke_batch', { stroke_id: strokeId, points: batch, color: '#1e88e5', width: 40, done })
        if (done) points = 0
      })
    }
    // Guessers: two wrong guesses, then the word, 10 to 30 s into the round. A player who knows
    // the word (a drawer, or one who guessed it) can't chat, so they skip.
    const guessing = (s) => state.round === roundNo && !state.closing && !s.knowsWord && s.guessedRound !== roundNo
    for (const s of sockets) {
      if (state.drawers.includes(s.player.id)) continue
      const at = 10000 + Math.floor(Math.random() * 20000)
      // Wrong guesses that can't be the word (the word list has only real words).
      later(at - 4000, () => guessing(s) && send(s, 'guess', { text: 'zzwrong' }))
      later(at - 2000, () => guessing(s) && send(s, 'guess', { text: 'qqnope' }))
      later(at, () => {
        if (!guessing(s) || !state.word) return
        s.guessSent = Date.now()
        send(s, 'guess', { text: state.word })
      })
    }
  }

  function onEvent(s, ev) {
    if (ev.seq > s.lastSeq) s.lastSeq = ev.seq
    // What this player sees, live or replayed.
    if (ev.type === 'stroke_batch' && ev.data.user_id !== s.player.id && !s.replaying) {
      // The stroke_id holds the stroke's start time, and a point's t_ms counts from that start,
      // so the start plus the last point's t_ms is when this batch was sent.
      const m = /^t(\d+)_/.exec(ev.data.stroke_id)
      if (m) {
        const pts = ev.data.points
        strokeFanout.add(Date.now() - (Number(m[1]) + pts[pts.length - 1][2]))
      }
    }
    if (ev.type === 'correct_guess' && ev.data.user_id === s.player.id) s.guessedRound = state.round
    // What the room does: each event once, by its seq (room-wide), from whichever socket has it
    // first, live or replayed after a resume.
    if (ev.seq <= state.seq) return
    state.seq = ev.seq
    switch (ev.type) {
      case 'host_changed':
        state.host = ev.data.user_id
        break
      case 'choosing':
        state.drawers = ev.data.drawer_ids
        state.word = ''
        for (const x of sockets) x.knowsWord = false
        break
      case 'round_started':
        state.drawers = ev.data.drawer_ids
        state.round = ev.data.round_no
        startDrawing(ev.data.round_no)
        break
      case 'round_ended':
        rounds.add(1)
        // Round numbers start again with every match.
        state.round = 0
        break
      case 'match_ended':
        matches.add(1)
        // The next match, after a short pause (the host can start from match_end).
        later(3000, startMatch)
        break
    }
  }

  function onPrivate(s, msg) {
    switch (msg.type) {
      case 'word_options':
        later(500, () => !state.closing && send(s, 'choose_word', { index: 0 }))
        break
      case 'your_word':
        state.word = msg.data.word
        s.knowsWord = true
        break
      case 'guess_result':
        if (msg.data.correct && s.guessSent) {
          guessResult.add(Date.now() - s.guessSent)
          s.guessSent = 0
        }
        break
      case 'error':
        // Expected now and then: a stroke or word choice that races the round's end.
        if (!['not_your_turn', 'bad_phase'].includes(msg.data.code)) {
          roomError('error:' + msg.data.code, msg.data.message)
        }
        break
    }
  }

  // A drop: a new ticket and hello with last_seq, after a backoff (design doc 7.4).
  function scheduleResume(s, code) {
    if (state.closing) return
    if (s.pinger !== null) clearInterval(s.pinger)
    s.pinger = null
    if (!s.droppedAt) s.droppedAt = Date.now()
    if (Date.now() - s.droppedAt > RESUME_GIVE_UP_MS) {
      roomError('resume_gave_up', `code ${code}`)
      closeAll()
      return
    }
    const backoff = Math.min(8000, 500 * 2 ** s.attempt) * (0.5 + Math.random() / 2)
    s.attempt++
    later(backoff, () => {
      if (state.closing) return
      // 503 while the lobby moves the room: try again later.
      const res = api('POST', `/api/lobby/rooms/${room.room_id}/join`, s.player.token, {})
      if (res.status >= 300) return scheduleResume(s, code)
      s.resumeCode = code
      open(s, res.json())
    })
  }

  function open(s, ticket) {
    const opened = Date.now()
    const ws = new WebSocket(WS_BASE + ticket.ws_url)
    s.ws = ws
    // On a resume the server replays the missed events, then sends resumed (or a fresh welcome).
    s.replaying = s.connectedOnce
    ws.onopen = () => send(s, 'hello', { ticket: ticket.ticket, last_seq: s.lastSeq })
    ws.onmessage = (e) => {
      if (s.ws !== ws) return
      const msg = JSON.parse(e.data)
      if (msg.type === 'welcome' || msg.type === 'resumed') {
        if (msg.type === 'welcome') {
          welcomeTime.add(Date.now() - opened)
          if (msg.data.seq > s.lastSeq) s.lastSeq = msg.data.seq
          if (msg.data.view && msg.data.view.host_id) state.host = msg.data.view.host_id
        }
        s.replaying = false
        if (s.droppedAt) {
          resumeTime.add(Date.now() - s.droppedAt)
          reconnects.add(1, { code: String(s.resumeCode) })
          s.droppedAt = 0
          s.attempt = 0
        }
        if (!s.connectedOnce) {
          s.connectedOnce = true
          welcomed++
          // The host is in: the others join. Everyone is in: the host starts.
          if (s.index === 0) for (let i = 1; i < PLAYERS; i++) join(sockets[i])
          if (welcomed === PLAYERS) later(500, startMatch)
        }
        if (s.pinger === null) s.pinger = setInterval(() => send(s, 'ping', { t: Date.now() }), 5000)
      } else if (msg.type === 'event') {
        onEvent(s, msg.data)
      } else if (msg.type === 'pong') {
        wsRtt.add(Date.now() - msg.data.t)
      } else if (msg.type === 'room_moved') {
        // Another server took the room over; the close (4010) that follows starts the resume.
      } else {
        onPrivate(s, msg)
      }
    }
    ws.onclose = (e) => {
      if (s.ws === ws && !state.closing) scheduleResume(s, e.code)
    }
    ws.onerror = () => {}
  }

  function join(s) {
    const t0 = Date.now()
    const res = api('POST', `/api/lobby/rooms/${room.room_id}/join`, s.player.token, {})
    if (res.status >= 300) {
      roomError('join', String(res.status))
      closeAll()
      return
    }
    joinTime.add(Date.now() - t0)
    open(s, res.json())
  }

  join(sockets[0])
  failures = 0
  later(ROOM_SECONDS * 1000, closeAll)
}
