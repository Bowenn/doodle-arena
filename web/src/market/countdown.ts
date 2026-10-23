// The time left until a drop opens or closes, as the drops page shows it.

/** "1:05" or "2 h 3 min" for a number of milliseconds. */
export function countdown(ms: number): string {
  const s = Math.max(0, Math.ceil(ms / 1000))
  if (s >= 3600) return `${Math.floor(s / 3600)} h ${Math.floor((s % 3600) / 60)} min`
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}
