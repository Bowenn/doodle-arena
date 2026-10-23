// A flash sale (task 6.8b, ADR 0022): the M6 criterion "a flash-sale load test shows no
// overselling".
//
//   scripts/loadtest.sh drops    make loadtest-drops (needs make seed: the buyers are seeded users)
//
// BUYERS seeded users (one per VU) race for EDITIONS editions of one drop. Every buyer joins the
// waiting room before the open (places are drawn by lottery at the open), waits for OPENS_AT,
// then tries to buy until they get an edition or a final answer: SOLD_OUT or ALREADY_BOUGHT.
// A buyer not admitted yet waits a second and tries again, as the web app's queue does.
// scripts/loadtest.sh then checks Postgres itself: exactly EDITIONS editions, exactly EDITIONS
// buyers charged, the authors paid, and the ledger balanced.
//
// k6 runs on the stack's network and calls the marketplace directly, like the tips test: through
// the gateway one address may make 20 requests a second.
//
//   drops_bought     201s: must be exactly EDITIONS
//   drops_refused    final refusals, tagged with the code (SOLD_OUT, ...)
//   drops_errors     anything unexpected (must be 0)
//   purchase_latency POST .../purchase until its answer
import http from 'k6/http'
import exec from 'k6/execution'
import { sleep } from 'k6'
import { Counter, Trend } from 'k6/metrics'

// TOKENS_0, TOKENS_1, ...: 100 tokens each (one environment string may not exceed 128 KB).
const TOKENS = []
for (let i = 0; __ENV[`TOKENS_${i}`]; i++) TOKENS.push(...JSON.parse(__ENV[`TOKENS_${i}`]))
const MARKET = __ENV.MARKET || 'http://marketplace:8080'
const DROP_ID = __ENV.DROP_ID
const OPENS_AT = Number(__ENV.OPENS_AT)
const EDITIONS = Number(__ENV.EDITIONS || 100)

const bought = new Counter('drops_bought')
const refused = new Counter('drops_refused')
const errors = new Counter('drops_errors')
const latency = new Trend('purchase_latency', true)

export const options = {
  scenarios: {
    buyers: {
      executor: 'per-vu-iterations',
      vus: Math.max(1, TOKENS.length),
      iterations: 1,
      maxDuration: '5m',
    },
  },
  thresholds: {
    drops_bought: [`count==${EDITIONS}`],
    drops_errors: ['count==0'],
    purchase_latency: ['p(95)<250'],
  },
  summaryTrendStats: ['p(50)', 'p(95)', 'p(99)', 'max'],
}

export function setup() {
  if (!DROP_ID || !OPENS_AT || TOKENS.length === 0) {
    exec.test.abort('scripts/loadtest.sh drops sets DROP_ID, OPENS_AT and TOKENS_0, TOKENS_1, ...')
  }
}

function headers(token, key) {
  const h = { Authorization: `Bearer ${token}` }
  if (key) h['Idempotency-Key'] = key
  return { headers: h, tags: { name: key ? 'POST purchase' : 'POST join' } }
}

export default function () {
  const buyer = TOKENS[(exec.vu.idInTest - 1) % TOKENS.length]
  const join = http.post(`${MARKET}/api/market/drops/${DROP_ID}/join`, null, headers(buyer.token))
  if (join.status !== 200) {
    errors.add(1, { step: 'join', code: String(join.status) })
    return
  }
  sleep(Math.max(0, (OPENS_AT - Date.now()) / 1000) + Math.random() * 0.2)

  for (let attempt = 0; attempt < 120; attempt++) {
    const res = http.post(`${MARKET}/api/market/drops/${DROP_ID}/purchase`, null, headers(buyer.token, crypto.randomUUID()))
    latency.add(res.timings.duration)
    if (res.status === 201) {
      bought.add(1)
      return
    }
    let code = String(res.status)
    try {
      code = res.json('error.code') || code
    } catch {
      // not JSON: keep the status
    }
    if (code === 'SOLD_OUT' || code === 'ALREADY_BOUGHT') {
      refused.add(1, { code })
      return
    }
    if (code === 'NOT_ADMITTED' || code === 'RATE_LIMITED' || code === 'DROP_CONFLICT' || code === 'DROP_NOT_OPEN') {
      // About a second, jittered as a real client would: otherwise every waiting buyer retries on
      // the same beat and the marketplace sees a burst every second.
      sleep(0.5 + Math.random())
      continue
    }
    errors.add(1, { step: 'purchase', code })
    return
  }
  errors.add(1, { step: 'purchase', code: 'GAVE_UP' })
}
