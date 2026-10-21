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
import exec from 'k6/execution'
import { sleep } from 'k6'
import { api, playRoom, PLAYERS, roomError } from './lib/room.js'

const TOKENS = JSON.parse(__ENV.TOKENS || '[]')
const SMOKE = __ENV.PROFILE === 'smoke'
// How long one room stays open: a few matches, then the virtual user leaves and starts over.
const ROOM_SECONDS = Number(__ENV.ROOM_SECONDS || (SMOKE ? 50 : 420))

export const options = {
  // PIN_HOST=icanfly.click PIN_IP=<address> skips the local resolver, which may still cache a
  // "no such name" from before the stack existed (a fresh stack's record is minutes old).
  hosts: __ENV.PIN_HOST ? { [__ENV.PIN_HOST]: __ENV.PIN_IP } : {},
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

  playRoom(room, players, ROOM_SECONDS)
  failures = 0
}
