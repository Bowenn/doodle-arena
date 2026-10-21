// One live room and thousands of viewers (task 6.1c, ADR 0017): the M6 criterion "one live room
// holds N thousand viewers with p99 broadcast latency under the target" (5,000 and 250 ms).
//
//   scripts/loadtest.sh live          5,000 viewers (make loadtest-live, about 15 minutes)
//   scripts/loadtest.sh live-smoke    50 viewers for half a minute (CI's stack job)
//
// setup() creates one live room. The room scenario plays it with four players for the whole test
// (loadtest/lib/room.js: two drawers stream a stroke batch every 50 ms). The viewers scenario
// connects VIEWERS viewers through the gateway, as the web app's watch page does: a viewer's
// ticket from POST /api/lobby/rooms/{id}/watch, then hello on the relay it names. Each VU holds
// many sockets (a VU per socket would need gigabytes), and they connect at RATE a second all
// together: each viewer costs two requests, and the gateway allows 20 a second per address.
// Then they all stay for HOLD seconds.
//
// The measure that counts is the relays' own relay.broadcast.latency (the owner's commit until
// the write to the viewer's socket returns; Grafana, "Game server", "Viewer broadcast latency").
// This script adds what a viewer feels, from the drawer's send:
//   viewer_fanout   a drawer's stroke batch until a viewer has it (sampled: one stroke batch in
//                   ten per socket, which keeps k6 itself from being the bottleneck)
//   viewer_welcome  connect until the welcome
//   viewer_gaps     events missing or out of order on a socket (must be 0)
//   viewer_drops    sockets closed by the server, tagged with the close code; each resumes
//   viewer_errors   tickets refused and the like, tagged with the step
import http from 'k6/http'
import exec from 'k6/execution'
import { WebSocket } from 'k6/websockets'
import { setInterval, setTimeout, clearInterval } from 'k6/timers'
import { Counter, Gauge, Trend } from 'k6/metrics'
import { api, GATEWAY, PLAYERS, playRoom, WS_BASE } from './lib/room.js'

// Four players, then the viewers' tokens: one per viewer VU (a relay keys viewers by socket, so
// one user may watch many times, and a short list fits in the environment).
const TOKENS = JSON.parse(__ENV.TOKENS || '[]')
const SMOKE = __ENV.PROFILE === 'live-smoke'
const VIEWERS = Number(__ENV.VIEWERS || (SMOKE ? 50 : 5000))
const VIEWER_VUS = Math.min(Number(__ENV.VIEWER_VUS || 50), VIEWERS)
const RATE = Number(__ENV.RATE || 9) // new viewers a second, all VUs together
const HOLD = Number(__ENV.HOLD || (SMOKE ? 30 : 300)) // seconds every viewer stays once all are in
const RAMP = Math.ceil(VIEWERS / RATE)
const ROOM_START = 15 // seconds for the room to fill and its first match to start
const TOTAL = ROOM_START + RAMP + HOLD
const SAMPLE = 10

const viewerFanout = new Trend('viewer_fanout', true)
const viewerWelcome = new Trend('viewer_welcome', true)
const viewerGaps = new Counter('viewer_gaps')
const viewerDrops = new Counter('viewer_drops')
const viewerErrors = new Counter('viewer_errors')
const viewerJoins = new Counter('viewer_joins')
const viewersOpen = new Gauge('viewers_open')

export const options = {
  hosts: __ENV.PIN_HOST ? { [__ENV.PIN_HOST]: __ENV.PIN_IP } : {},
  setupTimeout: '30s',
  scenarios: {
    room: {
      executor: 'per-vu-iterations', vus: 1, iterations: 1, exec: 'room',
      maxDuration: `${TOTAL + 60}s`,
    },
    viewers: {
      executor: 'per-vu-iterations', vus: VIEWER_VUS, iterations: 1, exec: 'viewers',
      startTime: `${ROOM_START}s`, maxDuration: `${RAMP + HOLD + 60}s`,
    },
  },
  thresholds: {
    // ADR 0017's target, as a viewer feels it (the relays' own histogram is the official one).
    viewer_fanout: ['p(99)<250'],
    viewer_gaps: ['count==0'],
    viewer_errors: ['count<10'],
  },
}

export function setup() {
  if (TOKENS.length < PLAYERS + 1) throw new Error('TOKENS needs four players and at least one viewer')
  const res = api('POST', '/api/lobby/rooms', TOKENS[0].token, { public: true, live: true })
  if (res.status >= 300) throw new Error(`creating the live room: HTTP ${res.status}`)
  const room = res.json()
  console.log(`live room ${room.room_id}: ${VIEWERS} viewers at ${RATE}/s, then ${HOLD} s`)
  return { room }
}

export function room(data) {
  playRoom(data.room, TOKENS.slice(0, PLAYERS), TOTAL)
}

export function viewers(data) {
  const vu = exec.vu.idInTest - 1 // the room's VU may have any id; only the count matters
  const viewerTokens = TOKENS.slice(PLAYERS)
  const token = viewerTokens[vu % viewerTokens.length].token
  // This VU's share, opened one at a time at RATE / VIEWER_VUS a second, staggered across VUs.
  const mine = Math.floor(VIEWERS / VIEWER_VUS) + (vu % VIEWER_VUS < VIEWERS % VIEWER_VUS ? 1 : 0)
  const gapMs = (1000 * VIEWER_VUS) / RATE
  const endAt = Date.now() + (RAMP + HOLD) * 1000
  const sockets = []
  let open = 0

  function connect(v) {
    const t0 = Date.now()
    const res = http.post(`${GATEWAY}/api/lobby/rooms/${data.room.room_id}/watch`, '{}', {
      headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' },
      tags: { name: '/api/lobby/rooms/{id}/watch' }, timeout: '10s',
    })
    if (res.status >= 300) {
      viewerErrors.add(1, { step: `watch_${res.status}` })
      return setTimeout(() => connect(v), 1000 + Math.random() * 2000)
    }
    const ticket = res.json()
    const ws = new WebSocket(WS_BASE + ticket.ws_url)
    v.ws = ws
    ws.onopen = () => ws.send(JSON.stringify({ type: 'hello', data: { ticket: ticket.ticket, last_seq: v.lastSeq } }))
    ws.onmessage = (e) => {
      const raw = e.data
      if (raw.startsWith('{"type":"event"')) {
        const seq = Number(/"seq":(\d+)/.exec(raw)[1])
        if (v.lastSeq && seq !== v.lastSeq + 1 && seq > v.lastSeq) viewerGaps.add(1)
        if (seq > v.lastSeq) v.lastSeq = seq
        if (raw.includes('"type":"stroke_batch"') && ++v.strokes % SAMPLE === 0) {
          // The stroke's start time rides in its id, and a point's t_ms counts from that start
          // (loadtest/lib/room.js): their sum is when the drawer sent this batch.
          const d = JSON.parse(raw).data.data
          const m = /^t(\d+)_/.exec(d.stroke_id)
          if (m) viewerFanout.add(Date.now() - (Number(m[1]) + d.points[d.points.length - 1][2]))
        }
        return
      }
      if (raw.startsWith('{"type":"welcome"')) {
        v.lastSeq = JSON.parse(raw).data.seq
        if (!v.welcomed) {
          v.welcomed = true
          viewerWelcome.add(Date.now() - t0)
          viewerJoins.add(1)
          viewersOpen.add(++open)
        }
      } else if (raw.startsWith('{"type":"error"')) {
        viewerErrors.add(1, { step: 'error' })
      }
    }
    ws.onclose = (e) => {
      if (v.ws !== ws || v.closing) return
      // Like the web app: a new ticket, then hello with the last seq.
      viewerDrops.add(1, { code: String(e.code) })
      setTimeout(() => connect(v), 500 + Math.random() * 1000)
    }
    ws.onerror = () => {}
  }

  let n = 0
  const start = setTimeout(function next() {
    if (n >= mine) return
    const v = { ws: null, lastSeq: 0, strokes: 0, welcomed: false, closing: false }
    sockets.push(v)
    n++
    connect(v)
    setTimeout(next, gapMs)
  }, (vu % VIEWER_VUS) * (1000 / RATE))
  const pinger = setInterval(() => {
    for (const v of sockets) if (v.ws && v.ws.readyState === 1) v.ws.send(JSON.stringify({ type: 'ping', data: { t: Date.now() } }))
  }, 5000)
  setTimeout(() => {
    clearInterval(pinger)
    for (const v of sockets) {
      v.closing = true
      if (v.ws) v.ws.close()
    }
  }, endAt - Date.now())
  void start
}
