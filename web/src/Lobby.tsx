// The lobby page: live rooms to watch, public rooms, create a room, quick play.

import { useCallback, useEffect, useState } from 'react'
import { createRoom, listRooms, LobbyError, quickPlay, type RoomListing } from './lobby.ts'

type Props = {
  onEnter: (roomId: string) => void
  /** Opens a live room as a viewer (ADR 0017). */
  onWatch: (roomId: string) => void
  onLeaderboard: () => void
  onGallery: () => void
}

export function Lobby({ onEnter, onWatch, onLeaderboard, onGallery }: Props) {
  const [rooms, setRooms] = useState<RoomListing[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [isPublic, setIsPublic] = useState(true)
  const [isLive, setIsLive] = useState(false)

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
        <button type="button" disabled={busy} onClick={() => void act(() => createRoom(isPublic, isLive))}>
          Create a room
        </button>
        <label>
          <input type="checkbox" checked={isPublic} onChange={(e) => setIsPublic(e.target.checked)} /> Public
        </label>
        <label title="Viewers can watch the room through the relays">
          <input type="checkbox" checked={isLive} onChange={(e) => setIsLive(e.target.checked)} /> Live
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
      {rooms && rooms.some((r) => r.live) && (
        <>
          <h2>Live now</h2>
          <ul className="room-list live-list">
            {rooms
              .filter((r) => r.live)
              .map((r) => (
                <li key={r.room_id}>
                  <span>
                    Room {r.room_id} · {r.players} playing · {watching(r.viewers)} · {r.phase}
                  </span>
                  <button type="button" className="primary" onClick={() => onWatch(r.room_id)}>
                    Watch
                  </button>
                </li>
              ))}
          </ul>
        </>
      )}
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
                {r.live && ` · live, ${watching(r.viewers)}`}
              </span>
              <button type="button" disabled={busy} onClick={() => onEnter(r.room_id)}>
                Join
              </button>
              {r.live && (
                <button type="button" onClick={() => onWatch(r.room_id)}>
                  Watch
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
    </main>
  )
}

function watching(n: number): string {
  return n === 1 ? '1 watching' : `${n} watching`
}

function message(e: unknown): string {
  if (e instanceof LobbyError) return e.message
  return 'Could not reach the lobby. Try again shortly.'
}
