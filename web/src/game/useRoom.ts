// React binding for a room: the connection lives outside React, the state in a reducer.

import { useCallback, useEffect, useReducer, useRef, useState } from 'react'
import { joinRoom } from '../lobby.ts'
import { recordWsRtt } from '../rum.ts'
import { RoomConnection, type ConnectionStatus } from './connection.ts'
import type { ClientMessage } from './protocol.ts'
import { roomReducer } from './roomState.ts'

export function useRoom(roomId: string) {
  const [state, dispatch] = useReducer(roomReducer, null)
  const [status, setStatus] = useState<ConnectionStatus>({ kind: 'connecting' })
  const [rtt, setRtt] = useState<number | null>(null)
  // Counts welcomes, so the canvas can drop local strokes the server never saw.
  const [welcomes, setWelcomes] = useState(0)
  const conn = useRef<RoomConnection | null>(null)

  useEffect(() => {
    // StrictMode mounts twice in development: the first connection closes before it is used,
    // which costs one unused ticket.
    const c = new RoomConnection(
      roomId,
      {
        welcome: (welcome) => {
          dispatch({ type: 'welcome', welcome })
          setWelcomes((n) => n + 1)
        },
        event: (event) => dispatch({ type: 'event', event }),
        private: (msg) => dispatch({ type: 'private', msg }),
        status: setStatus,
        rtt: (ms) => {
          setRtt(ms)
          recordWsRtt(ms)
        },
      },
      { join: joinRoom },
    )
    conn.current = c
    c.start()
    return () => {
      c.close()
      conn.current = null
    }
  }, [roomId])

  const send = useCallback((msg: ClientMessage) => conn.current?.send(msg) ?? false, [])

  return { state, status, rtt, welcomes, send }
}
