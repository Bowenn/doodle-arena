// The tips hot wallet under load (task 6.3, ADR 0021): many viewers tipping one round at once.
//
//   scripts/loadtest.sh tips    make loadtest-tips (needs make seed: the tippers are seeded users)
//
// TIPPERS seeded users (one per VU) tip round 1 of a fresh match RATE times a second in all, for
// DURATION seconds, 1 coin each. Every tip debits its tipper into one of 16 escrow shards, so no
// tip waits for the drawer's balance row. Afterwards scripts/loadtest.sh sends the round's
// RoundFinished and times the settlement that pays the drawer (tips.py settle).
//
// k6 runs on the stack's network and calls the marketplace directly: through the gateway one
// address may make 20 requests a second (limit_req), which a real crowd of viewers spreads over
// thousands of addresses. What this measures is the marketplace and Postgres.
//
//   tip_latency   POST /api/market/tips until its answer
//   tips_ok       201s; anything else is counted in tips_failed, tagged with the error code
import http from 'k6/http'
import exec from 'k6/execution'
import { check } from 'k6'
import { Counter, Trend } from 'k6/metrics'

// TOKENS_0, TOKENS_1, ...: 100 tokens each (one environment string may not exceed 128 KB).
const TOKENS = []
for (let i = 0; __ENV[`TOKENS_${i}`]; i++) TOKENS.push(...JSON.parse(__ENV[`TOKENS_${i}`]))
const MARKET = __ENV.MARKET || 'http://marketplace:8080'
const MATCH_ID = __ENV.MATCH_ID
const RATE = Number(__ENV.RATE || 300)
const DURATION = Number(__ENV.DURATION || 60)

const tipLatency = new Trend('tip_latency', true)
const tipsOk = new Counter('tips_ok')
const tipsFailed = new Counter('tips_failed')

export const options = {
  scenarios: {
    tips: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: `${DURATION}s`,
      preAllocatedVUs: TOKENS.length,
      maxVUs: TOKENS.length,
    },
  },
  thresholds: {
    tips_failed: ['count<1'],
    tip_latency: ['p(95)<250'],
  },
  summaryTrendStats: ['p(50)', 'p(95)', 'p(99)', 'max'],
}

export function setup() {
  if (!MATCH_ID || TOKENS.length === 0) {
    exec.test.abort('scripts/loadtest.sh tips sets MATCH_ID and TOKENS_0, TOKENS_1, ...')
  }
}

export default function () {
  const tipper = TOKENS[(exec.vu.idInTest - 1) % TOKENS.length]
  const res = http.post(
    `${MARKET}/api/market/tips`,
    JSON.stringify({ room_id: 'r_tips_load', match_id: MATCH_ID, round_no: 1, amount: 1 }),
    {
      headers: {
        Authorization: `Bearer ${tipper.token}`,
        'Content-Type': 'application/json',
        'Idempotency-Key': crypto.randomUUID(),
      },
      tags: { name: 'POST /api/market/tips' },
    },
  )
  tipLatency.add(res.timings.duration)
  if (check(res, { 'tip taken': (r) => r.status === 201 })) {
    tipsOk.add(1)
  } else {
    let code = String(res.status)
    try {
      code = res.json('error.code') || code
    } catch {
      // not JSON: keep the status
    }
    tipsFailed.add(1, { code })
  }
}
