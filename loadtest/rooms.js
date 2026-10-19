// Many small rooms (task 5.3): each virtual user plays one room of four players over four
// WebSockets, through the gateway like the web app does (design doc 6.3, 6.4, 7.2, 7.3).
//
//   scripts/loadtest.sh            the full ramp (10, 40, then 80 rooms)
//   scripts/loadtest.sh smoke      2 rooms for about a minute (CI's stack job)
//
// The script needs the players' access tokens in TOKENS (a JSON array, four per room, in
// order), which scripts/loadtest.sh mints and passes in the environment: never in a file.
//
// What a room does: the host creates a private room and connects first (whoever connects first
// becomes the room's host, design doc 7.3), then the other three join, and the host starts a
// match. With four players every round has two drawers (task 2.6a): the first picks a
// word, both send a stroke batch every 50 ms. The virtual user holds all four sockets, so it
// knows the word: after 10 to 30 s each guesser makes two wrong guesses, then the right one,
// and the round ends early, as in a real game. After a match the host starts the next one.
//
// Client-side metrics, in ms (sent to Grafana with --out opentelemetry, as k6_<name>):
//   stroke_fanout     a drawer's stroke batch until each other player receives it. The send time
//                     is in the stroke_id, so no protocol change is needed. All four sockets live
//                     in one process, so both ends use the same clock.
//   guess_result      a guess until its guess_result
//   ws_rtt            ping until pong: the network round trip alone
//   join              POST /join until the ticket arrives; welcome: connect until welcome
//   ws_closed         sockets the server closed, tagged with the close code
//   matches_completed, rounds_completed, room_errors
import http from 'k6/http'
import exec from 'k6/execution'
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

const strokeFanout = new Trend('stroke_fanout', true)
const guessResult = new Trend('guess_result', true)
const wsRtt = new Trend('ws_rtt', true)
const joinTime = new Trend('join', true)
const welcomeTime = new Trend('welcome', true)
const wsClosed = new Counter('ws_closed')
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
  const res = http.request(method, GATEWAY + path, body === undefined ? null : JSON.stringify(body), {
    headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' },
    tags: { name: path.replace(/rooms\/[a-z0-9]+/, 'rooms/{id}') },
    timeout: '10s',
  })
  if (res.status >= 300) {
    roomError(path.split('/').pop(), String(res.status))
    throw new Error(`${method} ${path}: ${res.status} ${res.body}`)
  }
  return res.json()
}

const send = (ws, type, data) => ws.send(JSON.stringify({ type, data }))

export default function () {
  // Four tokens per virtual user, in order: the first player is the host.
  const base = ((exec.vu.idInTest - 1) * PLAYERS) % TOKENS.length
  const players = TOKENS.slice(base, base + PLAYERS)
  if (players.length < PLAYERS) {
    throw new Error(`TOKENS has ${TOKENS.length} tokens; scripts/loadtest.sh mints four per room`)
  }

  const room = api('POST', '/api/lobby/rooms', players[0].token, { public: false })
  const tickets = players.map((p) => {
    const t0 = Date.now()
    const ticket = api('POST', `/api/lobby/rooms/${room.room_id}/join`, p.token, {})
    joinTime.add(Date.now() - t0)
    return ticket
  })

  const state = { word: '', drawers: [], round: 0, host: players[0].id, closing: false }
  const sockets = []
  const timers = []
  let welcomed = 0
  let strokeSeq = 0

  const later = (ms, fn) => timers.push(setTimeout(fn, ms))
  const every = (ms, fn) => {
    const id = setInterval(fn, ms)
    timers.push({ interval: id })
    return id
  }

  function closeAll() {
    state.closing = true
    for (const t of timers) {
      if (t && t.interval !== undefined) clearInterval(t.interval)
      else clearTimeout(t)
    }
    for (const s of sockets) s.ws.close()
  }

  function drawerSockets() {
    return sockets.filter((s) => state.drawers.includes(s.player.id))
  }

  function startDrawing(roundNo) {
    // Both drawers send a stroke batch every 50 ms until the round ends.
    for (const s of drawerSockets()) {
      let points = 0
      let strokeStarted = Date.now()
      let strokeId = ''
      const id = every(50, () => {
        if (state.round !== roundNo || state.closing) return clearInterval(id)
        if (points === 0) {
          strokeSeq++
          // The send time rides in the stroke_id, so receivers can time the fan-out.
          strokeId = `t${Date.now()}_${exec.vu.idInTest}_${strokeSeq}`
          strokeStarted = Date.now()
        }
        const now = Date.now()
        const batch = []
        for (let i = 0; i < 8; i++) {
          batch.push([Math.floor(Math.random() * 10000), Math.floor(Math.random() * 10000), now - strokeStarted])
        }
        points += batch.length
        const done = points >= 160
        send(s.ws, 'stroke_batch', { stroke_id: strokeId, points: batch, color: '#1e88e5', width: 40, done })
        s.lastBatchSent = now
        if (done) points = 0
      })
    }
    // Guessers: two wrong guesses, then the word, 10 to 30 s into the round.
    for (const s of sockets) {
      if (state.drawers.includes(s.player.id)) continue
      const at = 10000 + Math.floor(Math.random() * 20000)
      later(at - 4000, () => state.round === roundNo && !state.closing && send(s.ws, 'guess', { text: 'apple' }))
      later(at - 2000, () => state.round === roundNo && !state.closing && send(s.ws, 'guess', { text: 'banana' }))
      later(at, () => {
        if (state.round !== roundNo || state.closing || !state.word) return
        s.guessSent = Date.now()
        send(s.ws, 'guess', { text: state.word })
      })
    }
  }

  function onEvent(s, ev) {
    switch (ev.type) {
      case 'host_changed':
        state.host = ev.data.user_id
        break
      case 'choosing':
        state.drawers = ev.data.drawer_ids
        state.word = ''
        break
      case 'round_started':
        state.round = ev.data.round_no
        state.drawers = ev.data.drawer_ids
        // Each socket sees this event; the host's copy starts the room's drawing and guessing.
        if (s.index === 0) startDrawing(ev.data.round_no)
        break
      case 'stroke_batch': {
        if (ev.data.user_id === s.player.id) break
        // The stroke_id holds the stroke's start time, and a point's t_ms counts from that start,
        // so the start plus the last point's t_ms is when this batch was sent.
        const m = /^t(\d+)_/.exec(ev.data.stroke_id)
        if (m) {
          const pts = ev.data.points
          const sent = Number(m[1]) + pts[pts.length - 1][2]
          strokeFanout.add(Date.now() - sent)
        }
        break
      }
      case 'round_ended':
        if (s.index === 0) rounds.add(1)
        break
      case 'match_ended':
        if (s.index === 0) {
          matches.add(1)
          // The next match, after a short pause (the host can start from match_end).
          later(3000, startMatch)
        }
        break
    }
  }

  function onPrivate(s, msg) {
    switch (msg.type) {
      case 'word_options':
        later(500, () => !state.closing && send(s.ws, 'choose_word', { index: 0 }))
        break
      case 'your_word':
        state.word = msg.data.word
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

  function startMatch() {
    const host = sockets.find((x) => x.player.id === state.host)
    if (host && !state.closing) send(host.ws, 'start', {})
  }

  function connect(index) {
    const player = players[index]
    const opened = Date.now()
    const ws = new WebSocket(WS_BASE + tickets[index].ws_url)
    const s = { ws, player, index, guessSent: 0, lastBatchSent: 0 }
    sockets.push(s)

    ws.onopen = () => send(ws, 'hello', { ticket: tickets[index].ticket, last_seq: 0 })
    ws.onmessage = (e) => {
      const msg = JSON.parse(e.data)
      if (msg.type === 'welcome') {
        welcomeTime.add(Date.now() - opened)
        welcomed++
        if (index === 0) {
          state.host = msg.data.view.host_id || player.id
          for (let i = 1; i < PLAYERS; i++) connect(i)
        }
        // The host starts once everyone is in.
        if (welcomed === PLAYERS) later(500, startMatch)
      } else if (msg.type === 'event') {
        onEvent(s, msg.data)
      } else if (msg.type === 'pong') {
        wsRtt.add(Date.now() - msg.data.t)
      } else if (msg.type === 'room_moved') {
        roomError('room_moved', msg.data.reason)
      } else {
        onPrivate(s, msg)
      }
    }
    ws.onclose = (e) => {
      if (!state.closing) {
        wsClosed.add(1, { code: String(e.code) })
        roomError('closed:' + e.code, e.reason)
        closeAll()
      }
    }
    ws.onerror = (e) => {
      if (!state.closing) roomError('ws_error', e.error)
    }
    // The web app pings every 5 s; so does every bot.
    every(5000, () => !state.closing && send(ws, 'ping', { t: Date.now() }))
  }

  connect(0)

  later(ROOM_SECONDS * 1000, closeAll)
}
