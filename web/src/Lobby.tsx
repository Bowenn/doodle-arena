// The lobby page: public rooms, create a room, quick play.

import { useCallback, useEffect, useState } from 'react'
import { createRoom, listRooms, LobbyError, quickPlay, type RoomListing } from './lobby.ts'

type Props = { onEnter: (roomId: string) => void; onLeaderboard: () => void; onGallery: () => void }

export function Lobby({ onEnter, onLeaderboard, onGallery }: Props) {
  const [rooms, setRooms] = useState<RoomListing[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [isPublic, setIsPublic] = useState(true)

  const load = useCallback(() => {
    listRooms()
      .then((r) => setRooms(r.rooms))
      .catch((e: unknown) => setError(message(e)))
  }, [])

  useEffect(load, [load])

  async function act(fn: () => Promise<{ room_id: string }>) {
    setBusy(true)
    setError(null)
    try {
      onEnter((await fn()).room_id)
    } catch (e) {
      setError(message(e))
      setBusy(false)
    }
  }

  return (
    <main>
      <h1>Lobby</h1>
      <div className="actions">
        <button type="button" className="primary" disabled={busy} onClick={() => void act(quickPlay)}>
          Quick play
        </button>
        <button type="button" disabled={busy} onClick={() => void act(() => createRoom(isPublic))}>
          Create a room
        </button>
        <label>
          <input type="checkbox" checked={isPublic} onChange={(e) => setIsPublic(e.target.checked)} /> Public
        </label>
        <a
          href="/leaderboard"
          className="leaderboard-link"
          onClick={(e) => {
            e.preventDefault()
            onLeaderboard()
          }}
        >
          Weekly leaderboard
        </a>
        <a
          href="/gallery"
          className="leaderboard-link"
          onClick={(e) => {
            e.preventDefault()
            onGallery()
          }}
        >
          Drawing gallery
        </a>
      </div>
      {error && <p role="alert">{error}</p>}
      <h2>
        Open rooms{' '}
        <button type="button" onClick={load}>
          Refresh
        </button>
      </h2>
      {rooms === null ? (
        <p>Loading…</p>
      ) : rooms.length === 0 ? (
        <p>No open rooms. Create one!</p>
      ) : (
        <ul className="room-list">
          {rooms.map((r) => (
            <li key={r.room_id}>
              <span>
                Room {r.room_id} · {r.players}/{r.max_players} players · {r.phase}
              </span>
              <button type="button" disabled={busy} onClick={() => onEnter(r.room_id)}>
                Join
              </button>
            </li>
          ))}
        </ul>
      )}
    </main>
  )
}

function message(e: unknown): string {
  if (e instanceof LobbyError) return e.message
  return 'Could not reach the lobby. Try again shortly.'
}
