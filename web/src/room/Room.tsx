// The room page: canvas, word line and timer, scores, and the guess box. Everything shown comes
// from the server; buttons only send requests, and the server decides what happens.

import { useEffect, useRef, useState, type FormEvent } from 'react'
import type { ConnectionStatus } from '../game/connection.ts'
import { MAX_GUESS_CHARS, type ClientMessage } from '../game/protocol.ts'
import {
  canStart,
  choosingLabel,
  connectedCount,
  drawingLabel,
  isChooser,
  isDrawer,
  nameOf,
  wordLine,
  type FeedItem,
  type RoomState,
} from '../game/roomState.ts'
import { useRoom } from '../game/useRoom.ts'
import { Canvas } from './Canvas.tsx'

type Props = { roomId: string; onLeave: () => void }

export function Room({ roomId, onLeave }: Props) {
  const { state, status, rtt, welcomes, send } = useRoom(roomId)

  return (
    <main className="room">
      <div className="room-header">
        <h1>Room {roomId}</h1>
        <StatusLine status={status} rtt={rtt} />
        <button type="button" onClick={() => void copyLink(roomId)}>
          Copy invite link
        </button>
        <button type="button" onClick={onLeave}>
          Leave
        </button>
      </div>
      {status.kind === 'closed' && (
        <p role="alert" className="notice bad">
          {status.reason}
        </p>
      )}
      {state ? <RoomBody state={state} send={send} welcomes={welcomes} live={status.kind === 'open'} /> : <p>Joining…</p>}
    </main>
  )
}

async function copyLink(roomId: string) {
  const url = `${window.location.origin}/rooms/${roomId}`
  try {
    await navigator.clipboard.writeText(url)
  } catch {
    window.prompt('Copy this link:', url)
  }
}

function StatusLine({ status, rtt }: { status: ConnectionStatus; rtt: number | null }) {
  let text: string
  switch (status.kind) {
    case 'connecting':
      text = 'Connecting…'
      break
    case 'open':
      text = rtt === null ? 'Connected' : `Connected · ${rtt} ms`
      break
    case 'reconnecting':
      text = `Reconnecting (attempt ${status.attempt})…`
      break
    case 'closed':
      text = 'Disconnected'
      break
  }
  return (
    <span className={`status status-${status.kind}`} role="status">
      {text}
    </span>
  )
}

type BodyProps = {
  state: RoomState
  send: (m: ClientMessage) => boolean
  welcomes: number
  /** Whether the socket is up; a stale room can't be drawn on or guessed in. */
  live: boolean
}

function RoomBody({ state, send, welcomes, live }: BodyProps) {
  const drawing = state.phase === 'drawing'
  const canDraw = live && drawing && isDrawer(state)
  return (
    <div className="room-grid">
      <section className="stage">
        <PhaseBar state={state} send={send} />
        <Canvas strokes={state.strokes} me={state.me} canDraw={canDraw} send={send} resetKey={welcomes} />
      </section>
      <aside className="side">
        <Scoreboard state={state} />
        <Feed state={state} />
        <GuessBox state={state} send={send} live={live} />
      </aside>
    </div>
  )
}

function PhaseBar({ state, send }: { state: RoomState; send: (m: ClientMessage) => boolean }) {
  const start = canStart(state) && (
    <button type="button" className="primary" onClick={() => send({ type: 'start', data: {} })}>
      {state.phase === 'match_end' ? 'Play again' : 'Start the match'}
    </button>
  )

  let body
  switch (state.phase) {
    case 'waiting':
      body = (
        <>
          <span>
            {connectedCount(state) < 2
              ? 'Waiting for at least 2 players. Share the invite link.'
              : state.hostId === state.me
                ? 'Ready when you are.'
                : `Waiting for ${nameOf(state, state.hostId)} to start.`}
          </span>
          {start}
        </>
      )
      break
    case 'starting':
      body = <span>The match starts soon.</span>
      break
    case 'choosing':
      body =
        isChooser(state) && state.wordOptions ? (
          <>
            <span>Pick a word to draw:</span>
            {state.wordOptions.map((w, i) => (
              <button key={w} type="button" onClick={() => send({ type: 'choose_word', data: { index: i } })}>
                {w}
              </button>
            ))}
          </>
        ) : (
          <span>{isChooser(state) ? 'Getting your words…' : choosingLabel(state)}</span>
        )
      break
    case 'drawing':
      body = (
        <>
          <span className="word" aria-label="Word">
            {wordLine(state).split('').join(' ')}
          </span>
          <span className="muted">{drawingLabel(state)}</span>
        </>
      )
      break
    case 'reveal':
      body = (
        <>
          <span>
            The word was <strong>{state.word}</strong>.
          </span>
          <RoundPoints state={state} />
        </>
      )
      break
    case 'match_end':
      body = (
        <>
          <Results state={state} />
          {start}
        </>
      )
      break
  }

  return (
    <div className="phase-bar">
      <span className="round">
        {state.roundNo > 0 && state.phase !== 'match_end' && `Round ${state.roundNo}`}
      </span>
      {body}
      <Timer deadline={state.deadline} />
    </div>
  )
}

function RoundPoints({ state }: { state: RoomState }) {
  // A player who (re)joined during the reveal got a welcome, which has no round points.
  if (!state.roundPoints) return null
  const entries = Object.entries(state.roundPoints).sort((a, b) => b[1] - a[1])
  if (entries.length === 0) return <span className="muted">Nobody guessed it.</span>
  return (
    <span className="muted">
      {entries.map(([id, pts]) => `${nameOf(state, id)} +${pts}`).join(', ')}
    </span>
  )
}

function Results({ state }: { state: RoomState }) {
  return (
    <ol className="results" aria-label="Results">
      {(state.results ?? []).map((r) => (
        <li key={r.user_id}>
          #{r.placement} {r.name}: {r.score}
        </li>
      ))}
    </ol>
  )
}

/** Seconds left until the server's deadline. Display only: the server ends the phase. */
function Timer({ deadline }: { deadline: number | null }) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (deadline === null) return
    const t = setInterval(() => setNow(Date.now()), 250)
    return () => clearInterval(t)
  }, [deadline])
  if (deadline === null) return <span className="timer" />
  const left = Math.max(0, Math.ceil((deadline - now) / 1000))
  return (
    <span className="timer" aria-label="Time left">
      {left}s
    </span>
  )
}

function Scoreboard({ state }: { state: RoomState }) {
  const players = [...state.players].sort((a, b) => b.score - a.score)
  return (
    <section aria-label="Scores">
      <h2>Players</h2>
      <ul className="scores">
        {players.map((p) => (
          <li key={p.user_id} className={p.connected ? '' : 'away'}>
            <span className="name">
              {p.name}
              {p.user_id === state.me && ' (you)'}
            </span>
            <span className="tags">
              {p.user_id === state.hostId && <span title="Host">host</span>}
              {state.drawerIds.includes(p.user_id) && <span title="Drawing">drawing</span>}
              {p.guessed && <span title="Guessed the word">✓</span>}
              {!p.connected && <span>away</span>}
            </span>
            <span className="score">{p.score}</span>
          </li>
        ))}
      </ul>
    </section>
  )
}

function Feed({ state }: { state: RoomState }) {
  const list = useRef<HTMLUListElement>(null)
  useEffect(() => {
    // Scroll the list itself: scrollIntoView would also scroll the page, which moves the
    // canvas under the drawer's pointer in the middle of a stroke.
    const el = list.current
    if (el) el.scrollTop = el.scrollHeight
  }, [state.feed.length])
  return (
    <ul className="feed" aria-label="Chat" ref={list}>
      {state.feed.map((item, i) => (
        <li key={i} className={`feed-${item.kind}`}>
          {feedText(state, item)}
        </li>
      ))}
    </ul>
  )
}

function feedText(state: RoomState, item: FeedItem): string {
  switch (item.kind) {
    case 'chat':
      return `${nameOf(state, item.user_id)}: ${item.text}`
    case 'correct':
      return `${nameOf(state, item.user_id)} guessed the word!`
    case 'system':
      return item.text
  }
}

type GuessBoxProps = { state: RoomState; send: (m: ClientMessage) => boolean; live: boolean }

function GuessBox({ state, send, live }: GuessBoxProps) {
  const [text, setText] = useState('')
  const me = state.players.find((p) => p.user_id === state.me)
  const drawing = state.phase === 'drawing'
  // The server refuses these (cannot_chat_now); the box says so up front.
  const knowsWord =
    (drawing && (isDrawer(state) || !!me?.guessed)) || (state.phase === 'choosing' && isDrawer(state))
  const blocked = knowsWord || !live
  const notice = useNotice(state)

  function submit(e: FormEvent) {
    e.preventDefault()
    const t = text.trim()
    if (!t || blocked) return
    if (send({ type: 'guess', data: { text: t } })) setText('')
  }

  return (
    <form className="guess" onSubmit={submit}>
      {notice && (
        <p className={`notice ${notice.tone}`} role="status">
          {notice.text}
        </p>
      )}
      <input
        aria-label={drawing ? 'Guess' : 'Chat'}
        placeholder={!live ? 'Not connected' : knowsWord ? 'You know the word' : drawing ? 'Type your guess' : 'Chat'}
        value={text}
        maxLength={MAX_GUESS_CHARS}
        disabled={blocked}
        onChange={(e) => setText(e.target.value)}
      />
      <button type="submit" disabled={blocked}>
        Send
      </button>
    </form>
  )
}

/** The latest notice, hidden again after a few seconds. */
function useNotice(state: RoomState) {
  const [hidden, setHidden] = useState<number | null>(null)
  const notice = state.notice
  useEffect(() => {
    if (!notice) return
    const t = setTimeout(() => setHidden(notice.id), 3000)
    return () => clearTimeout(t)
  }, [notice])
  return notice && notice.id !== hidden ? notice : null
}
