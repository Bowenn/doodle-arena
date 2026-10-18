/** How a player id is shown when the gallery has no display name (the leaderboard does the same). */
export function playerLabel(id: string): string {
  return `Player ${id.slice(0, 8)}`
}

export const STATUS_LABEL: Record<string, string> = {
  processing: 'Being processed',
  review: 'Waiting for a moderator',
  approved: 'Approved',
  rejected: 'Rejected',
  failed: "Couldn't be processed",
}
