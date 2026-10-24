// One room of four players, as loadtest/rooms.js (task 5.3) and loadtest/live.js (task 6.1c)
// play it: the host connects first, the others join, the host starts matches until the room's
// time is up; drawers stream strokes, guessers guess, dropped sockets resume like the web app.
// The client-side metrics are described in loadtest/rooms.js.
import http from 'k6/http'
import exec from 'k6/execution'
import { WebSocket } from 'k6/websockets'
import { setInterval, setTimeout, clearInterval, clearTimeout } from 'k6/timers'
import { Counter, Trend } from 'k6/metrics'
import { STROKES_SUBPROTOCOL, decodeStrokeEvent, encodeStrokeBatch } from './strokeCodec.js'

export const GATEWAY = __ENV.GATEWAY || 'http://localhost:8080'
export const WS_BASE = GATEWAY.replace(/^http/, 'ws')
export const PLAYERS = 4
// A dropped socket retries for about as long as the web app does.
const RESUME_GIVE_UP_MS = 60000
// Task 6.4 (ADR 0025): how often drawers send a batch, how many points a second they draw (a
// steady pointer, so a shorter interval sends smaller batches, not more ink), and the wire
// format: json, or pb for binary stroke frames (subprotocol doodle.strokes.pb).
const BATCH_MS = Number(__ENV.BATCH_MS || 50)
const POINTS_PER_SECOND = Number(__ENV.POINTS_PER_SECOND || 160)
const WIRE = __ENV.WIRE === 'pb' ? 'pb' : 'json'

const strokeFanout = new Trend('stroke_fanout', true)
const inkDelay = new Trend('ink_delay', true)
const bytesSent = new Counter('ws_bytes_sent')
const bytesReceived = new Counter('ws_bytes_received')
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
export function roomError(step, detail) {
  roomErrors.add(1, { step })
  console.warn(`room error (VU ${exec.vu.idInTest}): ${step}${detail ? ': ' + detail : ''}`)
}

export function api(method, path, token, body) {
  return http.request(method, GATEWAY + path, body === undefined ? null : JSON.stringify(body), {
    headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' },
    tags: { name: path.replace(/rooms\/[a-z0-9]+/, 'rooms/{id}') },
    timeout: '10s',
  })
}

export const send = (s, type, data) => {
  if (!s.ws || s.ws.readyState !== 1) return false
  const stroke = type === 'stroke_batch'
  if (stroke && WIRE === 'pb') {
    // The game server always accepts the subprotocol it was asked for.
    const frame = encodeStrokeBatch(data)
    bytesSent.add(frame.byteLength, { kind: 'stroke' })
    s.ws.send(frame)
    return true
  }
  const text = JSON.stringify({ type, data })
  bytesSent.add(text.length, { kind: stroke ? 'stroke' : 'other' })
  s.ws.send(text)
  return true
}

/** Plays room (the lobby's create response) with four players ({id, token}, the host first)
 * for seconds, then closes every socket. */
export function playRoom(room, players, seconds) {
  const state = { word: '', drawers: [], round: 0, seq: 0, drawToken: 0, host: players[0].id, closing: false }
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
    // Both drawers send a stroke batch every BATCH_MS until the round ends. Each round gets its own
    // token, so a drawing loop from an earlier round (round numbers repeat every match, and a
    // takeover can replay a round's start) stops instead of drawing twice as fast.
    const token = ++state.drawToken
    for (const s of sockets.filter((x) => state.drawers.includes(x.player.id))) {
      let points = 0
      let owed = 0 // points drawn since the last batch, at POINTS_PER_SECOND
      let strokeStarted = 0
      let strokeId = ''
      const id = every(BATCH_MS, () => {
        if (state.drawToken !== token || state.round !== roundNo || state.closing) return clearInterval(id)
        const now = Date.now()
        if (points === 0) {
          strokeSeq++
          // The stroke's start time rides in its id, so receivers can time the fan-out.
          strokeId = `t${now}_${exec.vu.idInTest}_${strokeSeq}`
          strokeStarted = now
        }
        owed += (POINTS_PER_SECOND * BATCH_MS) / 1000
        const n = Math.floor(owed)
        if (n < 1) return
        owed -= n
        // The points were drawn evenly over the last interval; t_ms is each one's own time.
        const batch = []
        for (let i = 0; i < n; i++) {
          const t = Math.max(0, now - strokeStarted - Math.round(((n - 1 - i) * BATCH_MS) / n))
          batch.push([Math.floor(Math.random() * 10000), Math.floor(Math.random() * 10000), t])
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
        const start = Number(m[1])
        strokeFanout.add(Date.now() - (start + pts[pts.length - 1][2]))
        // The batch's oldest point: how long ink waited, including its wait in the batch.
        inkDelay.add(Date.now() - (start + pts[0][2]))
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
    const ws = WIRE === 'pb' ? new WebSocket(WS_BASE + ticket.ws_url, [STROKES_SUBPROTOCOL]) : new WebSocket(WS_BASE + ticket.ws_url)
    ws.binaryType = 'arraybuffer'
    s.ws = ws
    // On a resume the server replays the missed events, then sends resumed (or a fresh welcome).
    s.replaying = s.connectedOnce
    ws.onopen = () => send(s, 'hello', { ticket: ticket.ticket, last_seq: s.lastSeq })
    ws.onmessage = (e) => {
      if (s.ws !== ws) return
      if (typeof e.data !== 'string') {
        // A binary frame is a stroke_batch event (ADR 0025).
        bytesReceived.add(e.data.byteLength, { kind: 'stroke' })
        const ev = decodeStrokeEvent(e.data)
        if (ev) onEvent(s, ev)
        return
      }
      const msg = JSON.parse(e.data)
      bytesReceived.add(e.data.length, { kind: msg.type === 'event' && msg.data.type === 'stroke_batch' ? 'stroke' : 'other' })
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
  later(seconds * 1000, closeAll)
}
