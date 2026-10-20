// Real-user monitoring (task 5.4, docs/observability.md): the Core Web Vitals and the room's
// WebSocket round trip, sent as OpenTelemetry metrics through the gateway (/otlp/v1/metrics).
// This part loads with the app and is tiny; the OpenTelemetry SDK (telemetry.ts) loads only
// after the page has loaded, so measuring the page doesn't slow it down.

/** The page a path belongs to, as metrics name it: a route, never an id (a room id or a
 * drawing id in an attribute would make one series per room). The collector's browser
 * receiver accepts exactly these names and turns anything else into "other". */
export function pageName(pathname: string): string {
  if (pathname === '/') return 'lobby'
  if (pathname.startsWith('/rooms/')) return 'room'
  const page = pathname.replace(/^\//, '').replace(/\/$/, '')
  return PAGES.has(page) ? page : 'other'
}

const PAGES = new Set(['gallery', 'market', 'my-drawings', 'wallet', 'developer', 'moderate', 'leaderboard'])

type Recorder = { rtt: (ms: number) => void }

// Round trips measured before the SDK has loaded wait here (a room opened from an invite link
// pings within seconds); the cap keeps a page whose SDK never loads from growing it forever.
const MAX_QUEUED = 50
let queued: number[] = []
let recorder: Recorder | null = null

/** Records one ping round trip of the room's WebSocket (web.ws.rtt). */
export function recordWsRtt(ms: number): void {
  if (recorder) recorder.rtt(ms)
  else if (queued.length < MAX_QUEUED) queued.push(ms)
}

/** Hands queued round trips to the loaded SDK (telemetry.ts calls it once). */
export function attach(r: Recorder): void {
  recorder = r
  for (const ms of queued) r.rtt(ms)
  queued = []
}

/** Starts RUM once the page has loaded. Off in tests and when VITE_RUM is "off". */
export function startRum(): void {
  if (import.meta.env.MODE === 'test' || import.meta.env.VITE_RUM === 'off') return
  const load = () => {
    void import('./telemetry.ts')
      .then((m) => m.start())
      .catch(() => {}) // telemetry must never break the app
  }
  if (document.readyState === 'complete') setTimeout(load, 0)
  else window.addEventListener('load', () => setTimeout(load, 0), { once: true })
}

/** For tests: forget the recorder and the queue. */
export function resetRumForTests(): void {
  recorder = null
  queued = []
}
