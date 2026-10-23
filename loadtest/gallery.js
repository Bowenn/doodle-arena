// The gallery with and without its cache (task 6.7b, ADR 0024).
//
//   scripts/loadtest.sh gallery    make loadtest-gallery (needs make seed: 300,000 drawings)
//
// RATE requests a second for DURATION seconds, the mix of real page views:
//   50%  the gallery's first page                 GET /api/assets/drawings
//   20%  its second or third page (the cursor this VU last saw, as a player clicking "more")
//   20%  a Market page's lookup of 20 drawings    GET /api/assets/drawings?ids=...
//   10%  one drawing's page                       GET /api/assets/drawings/{id}
// The ids are a random sample of the seeded approved drawings (IDS). Meanwhile
// scripts/loadtest.sh approves a new drawing every 2 seconds, so the gallery's generation keeps
// moving as it would in play. CACHE (on or off) only labels the run: scripts/loadtest.sh sets
// ASSETS_CACHE on the assets container.
//
// k6 runs on the stack's network and calls assets directly: through the gateway one address may
// make 20 requests a second (limit_req). What this measures is assets, Redis and Postgres.
import http from 'k6/http'
import exec from 'k6/execution'
import { check } from 'k6'
import { Counter, Trend } from 'k6/metrics'

const TOKENS = JSON.parse(__ENV.TOKENS || '[]')
const IDS = (__ENV.IDS || '').split(',').filter((id) => id)
const ASSETS = __ENV.ASSETS || 'http://assets:8080'
const RATE = Number(__ENV.RATE || 200)
const DURATION = Number(__ENV.DURATION || 120)
const API = `${ASSETS}/api/assets/drawings`

const latency = {
  first: new Trend('gallery_first_page', true),
  more: new Trend('gallery_next_page', true),
  ids: new Trend('market_ids_lookup', true),
  one: new Trend('drawing_page', true),
}
const failed = new Counter('requests_failed')

export const options = {
  scenarios: {
    gallery: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: `${DURATION}s`,
      preAllocatedVUs: 100,
      maxVUs: 400,
    },
  },
  thresholds: {
    requests_failed: ['count<1'],
  },
  summaryTrendStats: ['p(50)', 'p(95)', 'p(99)', 'max'],
  tags: { cache: __ENV.CACHE || 'on' },
}

export function setup() {
  if (TOKENS.length === 0 || IDS.length < 20) {
    exec.test.abort('scripts/loadtest.sh gallery sets TOKENS and IDS')
  }
}

// The cursors this VU last saw: page 2's and page 3's.
const cursors = []

function get(url, kind, name) {
  const user = TOKENS[(exec.vu.idInTest - 1) % TOKENS.length]
  const res = http.get(url, { headers: { Authorization: `Bearer ${user.token}` }, tags: { name } })
  latency[kind].add(res.timings.duration)
  if (!check(res, { 'answered 200': (r) => r.status === 200 })) {
    failed.add(1, { kind, status: String(res.status) })
    return null
  }
  return res.json()
}

function sample(n) {
  const out = []
  for (let i = 0; i < n; i++) out.push(IDS[Math.floor(Math.random() * IDS.length)])
  return out
}

export default function () {
  const roll = Math.random()
  if (roll < 0.5) {
    const page = get(API, 'first', 'gallery page 1')
    if (page && page.next_cursor) cursors[0] = page.next_cursor
  } else if (roll < 0.7) {
    const deeper = cursors.length > 1 && Math.random() < 0.5 ? 1 : 0
    if (!cursors[deeper]) {
      const page = get(API, 'first', 'gallery page 1')
      if (page && page.next_cursor) cursors[0] = page.next_cursor
      return
    }
    const page = get(`${API}?cursor=${cursors[deeper]}`, 'more', 'gallery page 2 or 3')
    if (page && page.next_cursor && deeper === 0) cursors[1] = page.next_cursor
  } else if (roll < 0.9) {
    get(`${API}?ids=${sample(20).join(',')}`, 'ids', 'ids lookup')
  } else {
    get(`${API}/${sample(1)[0]}`, 'one', 'drawing page')
  }
}
