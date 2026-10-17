// The weekly leaderboard: the sum of match scores per ISO week, ranked by the lobby.

import { useEffect, useState } from 'react'
import { getLeaderboard, LobbyError, type Board } from './lobby.ts'
import { isoWeek, shiftWeek } from './weeks.ts'

type Props = { meId: string; onBack: () => void }

export function Leaderboard({ meId, onBack }: Props) {
  // undefined: the current week, as the server sees it.
  const [week, setWeek] = useState<string | undefined>(undefined)
  const [board, setBoard] = useState<Board | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [thisWeek] = useState(() => isoWeek(new Date()))

  useEffect(() => {
    let cancelled = false
    getLeaderboard(week)
      .then((b) => {
        if (cancelled) return
        setBoard(b)
        setError(null)
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(message(e))
      })
    return () => {
      cancelled = true
    }
  }, [week])

  const shown = board?.week ?? week
  const meShown = board?.entries.some((e) => e.user_id === meId) ?? false

  return (
    <main>
      <h1>Weekly leaderboard</h1>
      <div className="actions">
        <button type="button" onClick={onBack}>
          Back to lobby
        </button>
        <button type="button" disabled={!shown} onClick={() => shown && setWeek(shiftWeek(shown, -1))}>
          Previous week
        </button>
        <strong>{shown ?? '…'}</strong>
        <button
          type="button"
          disabled={!shown || shown >= thisWeek}
          onClick={() => shown && setWeek(shiftWeek(shown, 1))}
        >
          Next week
        </button>
      </div>
      <p className="muted">Points from every finished match, Monday to Sunday (UTC).</p>
      {error && <p role="alert">{error}</p>}
      {board === null ? (
        !error && <p>Loading…</p>
      ) : board.entries.length === 0 ? (
        <p>No matches finished in {board.week} yet.</p>
      ) : (
        <table className="leaderboard">
          <thead>
            <tr>
              <th scope="col">Rank</th>
              <th scope="col">Player</th>
              <th scope="col">Points</th>
            </tr>
          </thead>
          <tbody>
            {board.entries.map((e) => {
              const mine = e.user_id === meId
              return (
                <tr key={e.user_id} className={mine ? 'me' : undefined} aria-current={mine ? 'true' : undefined}>
                  <td>{e.rank}</td>
                  <td>
                    {e.name || `Player ${e.user_id.slice(0, 8)}`}
                    {mine && ' (you)'}
                  </td>
                  <td>{e.score}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}
      {board?.me && !meShown && (
        <p>
          You are #{board.me.rank} with {board.me.score} points.
        </p>
      )}
      {board && !board.me && <p className="muted">You haven't scored in {board.week}.</p>}
    </main>
  )
}

function message(e: unknown): string {
  if (e instanceof LobbyError) return e.message
  return 'Could not reach the lobby. Try again shortly.'
}
