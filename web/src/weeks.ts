// ISO weeks in UTC, the way the lobby names leaderboard weeks (such as 2026-W42).

const DAY = 24 * 60 * 60 * 1000

/** The ISO week of a moment, in UTC, such as 2026-W42 (the server's format). */
export function isoWeek(d: Date): string {
  // An ISO week belongs to the year that holds its Thursday.
  const mondayBased = (d.getUTCDay() + 6) % 7
  const thursday = new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() + 3 - mondayBased))
  const year = thursday.getUTCFullYear()
  const week = 1 + Math.floor((thursday.getTime() - Date.UTC(year, 0, 1)) / (7 * DAY))
  return `${year}-W${String(week).padStart(2, '0')}`
}

/** The week `by` weeks before (negative) or after an ISO week. */
export function shiftWeek(week: string, by: number): string {
  const m = /^(\d{4})-W(\d{2})$/.exec(week)
  if (!m) return week
  // January 4 is always in week 1.
  const jan4 = Date.UTC(Number(m[1]), 0, 4)
  const monday1 = jan4 - ((new Date(jan4).getUTCDay() + 6) % 7) * DAY
  return isoWeek(new Date(monday1 + (Number(m[2]) - 1 + by) * 7 * DAY))
}
