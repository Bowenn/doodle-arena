// Room state on the client: a pure reducer over what the server sends. The client never decides
// an outcome (who is right, who scores, when a phase ends); it only mirrors the server.
//
// welcome replaces the state; after it, shared events apply in seq order and an event the state
// already contains (seq <= state.seq) is ignored.

import type {
  CanvasBatch,
  Phase,
  PlayerView,
  PrivateMessage,
  Result,
  RoomEvent,
  Welcome,
} from './protocol.ts'

export type FeedItem =
  | { kind: 'chat'; user_id: string; text: string }
  | { kind: 'correct'; user_id: string; order: number }
  | { kind: 'system'; text: string }

export type Notice = { id: number; text: string; tone: 'good' | 'bad' | 'info' }

export type RoomState = {
  me: string
  seq: number
  roomId: string
  phase: Phase
  hostId: string
  matchId: string | null
  settings: Record<string, number>
  players: PlayerView[]
  roundNo: number
  drawerIds: string[]
  /** End of the current phase (server Unix ms), or null when the phase has none. */
  deadline: number | null
  wordLength: number
  /** Letters revealed so far, '_' for hidden ones (guessers, while drawing). */
  mask: string
  /** The word, once the round has ended. */
  word: string | null
  /** The word for the drawer while drawing (private your_word). */
  myWord: string | null
  /** Word options for the drawer while choosing (private word_options). */
  wordOptions: string[] | null
  strokes: CanvasBatch[]
  roundPoints: Record<string, number> | null
  results: Result[] | null
  feed: FeedItem[]
  notice: Notice | null
}

const FEED_LIMIT = 200

export type Action =
  | { type: 'welcome'; welcome: Welcome }
  | { type: 'event'; event: RoomEvent }
  | { type: 'private'; msg: PrivateMessage }

export function roomReducer(state: RoomState | null, action: Action): RoomState | null {
  switch (action.type) {
    case 'welcome':
      return fromWelcome(action.welcome, state)
    case 'event':
      if (!state || action.event.seq <= state.seq) return state
      return { ...applyEvent(state, action.event), seq: action.event.seq }
    case 'private':
      return state && applyPrivate(state, action.msg)
  }
}

/** Builds the state from a welcome. The chat feed isn't part of the server's view, so a
 * reconnect keeps the feed it had. */
export function fromWelcome(w: Welcome, prev: RoomState | null): RoomState {
  const v = w.view
  const feed = prev && prev.roomId === v.room_id ? prev.feed : []
  return {
    me: w.user_id,
    seq: w.seq,
    roomId: v.room_id,
    phase: v.phase,
    hostId: v.host_id,
    matchId: v.match_id ?? null,
    settings: v.settings,
    players: v.players ?? [],
    roundNo: v.round_no ?? 0,
    drawerIds: v.drawer_ids ?? [],
    deadline: v.deadline ?? null,
    wordLength: v.word_length ?? 0,
    mask: v.mask ?? '',
    word: v.word ?? null,
    myWord: null, // your_word follows the welcome when this player is drawing
    wordOptions: null, // word_options follows the welcome when this player is choosing
    strokes: v.strokes ?? [],
    roundPoints: null,
    results: null,
    feed,
    notice: null,
  }
}

function applyEvent(s: RoomState, e: RoomEvent): RoomState {
  switch (e.type) {
    case 'player_joined': {
      const known = s.players.some((p) => p.user_id === e.data.user_id)
      const players = known
        ? updatePlayer(s.players, e.data.user_id, { connected: true, name: e.data.name })
        : [...s.players, { user_id: e.data.user_id, name: e.data.name, score: 0, connected: true, guessed: false }]
      return { ...s, players, feed: addFeed(s.feed, { kind: 'system', text: `${e.data.name} joined` }) }
    }
    case 'player_left':
      return {
        ...s,
        players: updatePlayer(s.players, e.data.user_id, { connected: false }),
        feed: addFeed(s.feed, { kind: 'system', text: `${nameOf(s, e.data.user_id)} left` }),
      }
    case 'player_removed':
      return { ...s, players: s.players.filter((p) => p.user_id !== e.data.user_id) }
    case 'host_changed':
      return { ...s, hostId: e.data.user_id }
    case 'match_started':
      return {
        ...s,
        phase: 'starting',
        matchId: e.data.match_id,
        deadline: e.data.deadline,
        roundNo: 0,
        results: null,
        roundPoints: null,
        players: s.players.map((p) => ({ ...p, score: 0 })),
        feed: addFeed(s.feed, { kind: 'system', text: 'The match is starting' }),
      }
    case 'choosing':
      return {
        ...newRound(s),
        phase: 'choosing',
        roundNo: e.data.round_no,
        drawerIds: e.data.drawer_ids,
        deadline: e.data.deadline,
      }
    case 'round_started':
      return {
        ...s,
        phase: 'drawing',
        roundNo: e.data.round_no,
        drawerIds: e.data.drawer_ids,
        deadline: e.data.deadline,
        wordLength: e.data.word_length,
        mask: '_'.repeat(e.data.word_length),
        wordOptions: null,
      }
    case 'stroke_batch':
      return { ...s, strokes: [...s.strokes, e.data] }
    case 'stroke_undone':
      return {
        ...s,
        strokes: s.strokes.filter(
          (b) => !(b.stroke_id === e.data.stroke_id && b.user_id === e.data.user_id),
        ),
      }
    case 'hint':
      return { ...s, mask: e.data.mask }
    case 'chat':
      return { ...s, feed: addFeed(s.feed, { kind: 'chat', user_id: e.data.user_id, text: e.data.text }) }
    case 'correct_guess':
      return {
        ...s,
        players: updatePlayer(s.players, e.data.user_id, { guessed: true }),
        feed: addFeed(s.feed, { kind: 'correct', user_id: e.data.user_id, order: e.data.order }),
      }
    case 'round_ended': {
      const scores = new Map(e.data.scores.map((l) => [l.user_id, l.score]))
      return {
        ...s,
        phase: 'reveal',
        deadline: null, // the server times the reveal; the next event ends it
        word: e.data.word,
        roundPoints: e.data.round_points,
        players: s.players.map((p) => ({ ...p, score: scores.get(p.user_id) ?? p.score })),
        feed: addFeed(s.feed, { kind: 'system', text: `The word was "${e.data.word}"` }),
      }
    }
    case 'match_ended': {
      const scores = new Map(e.data.results.map((r) => [r.user_id, r.score]))
      return {
        ...s,
        phase: 'match_end',
        deadline: null,
        drawerIds: [],
        myWord: null,
        results: e.data.results,
        players: s.players.map((p) => ({ ...p, score: scores.get(p.user_id) ?? p.score })),
      }
    }
    default:
      return s // an event type this client doesn't know yet
  }
}

/** What the server clears when the next drawer starts choosing. */
function newRound(s: RoomState): RoomState {
  return {
    ...s,
    strokes: [],
    word: null,
    myWord: null,
    wordOptions: null,
    mask: '',
    wordLength: 0,
    roundPoints: null,
    players: s.players.map((p) => ({ ...p, guessed: false })),
  }
}

let noticeId = 0

function applyPrivate(s: RoomState, m: PrivateMessage): RoomState {
  switch (m.type) {
    case 'word_options':
      return { ...s, wordOptions: m.data.words }
    case 'your_word':
      return { ...s, myWord: m.data.word }
    case 'guess_result':
      return m.data.correct
        ? notify(s, 'You guessed it!', 'good')
        : s // a wrong guess shows up as chat (or stays private when it contains the word)
    case 'rate_limited':
      return notify(s, 'Slow down: too many guesses.', 'bad')
    case 'error':
      return notify(s, m.data.message, 'bad')
    default:
      return s
  }
}

function notify(s: RoomState, text: string, tone: Notice['tone']): RoomState {
  return { ...s, notice: { id: ++noticeId, text, tone } }
}

function updatePlayer(players: PlayerView[], id: string, patch: Partial<PlayerView>): PlayerView[] {
  return players.map((p) => (p.user_id === id ? { ...p, ...patch } : p))
}

function addFeed(feed: FeedItem[], item: FeedItem): FeedItem[] {
  const next = [...feed, item]
  return next.length > FEED_LIMIT ? next.slice(next.length - FEED_LIMIT) : next
}

export function nameOf(s: RoomState, id: string): string {
  return s.players.find((p) => p.user_id === id)?.name ?? 'Someone'
}

export function isDrawer(s: RoomState): boolean {
  return s.drawerIds.includes(s.me)
}

export function connectedCount(s: RoomState): number {
  return s.players.filter((p) => p.connected).length
}

/** Whether to offer the Start button. The server still decides; this only hides a button
 * that would be refused. */
export function canStart(s: RoomState): boolean {
  return (
    s.hostId === s.me && (s.phase === 'waiting' || s.phase === 'match_end') && connectedCount(s) >= 2
  )
}

/** The word line to show: the drawer's word, the revealed word, or the hint mask. */
export function wordLine(s: RoomState): string {
  if (s.word) return s.word
  if (s.myWord) return s.myWord
  return s.mask
}
