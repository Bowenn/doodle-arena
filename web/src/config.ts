// Client settings that experiments can tune.

const DEFAULT_STROKE_BATCH_MS = 50
const MIN_STROKE_BATCH_MS = 10
const MAX_STROKE_BATCH_MS = 1000

/** How often the canvas sends stroke points. VITE_STROKE_BATCH_MS sets the default for a
 * deployment; ?batch_ms=<n> in the page URL overrides it for one experiment. */
export function strokeBatchMs(search: string = window.location.search): number {
  const fromUrl = new URLSearchParams(search).get('batch_ms')
  const fromEnv = import.meta.env.VITE_STROKE_BATCH_MS as string | undefined
  for (const raw of [fromUrl, fromEnv]) {
    const n = Number(raw)
    if (raw && Number.isInteger(n)) {
      return Math.min(MAX_STROKE_BATCH_MS, Math.max(MIN_STROKE_BATCH_MS, n))
    }
  }
  return DEFAULT_STROKE_BATCH_MS
}
