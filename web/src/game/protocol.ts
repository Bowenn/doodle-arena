// The WebSocket protocol (design doc, section 6.4). Every frame is JSON {type, data}. These
// types mirror the game server's messages; the server is the source of truth.

export type Phase = 'waiting' | 'starting' | 'choosing' | 'drawing' | 'reveal' | 'match_end'

/** A point is [x, y, t_ms]: coordinates normalized to 0..10000, t_ms since the stroke began. */
export type Point = [number, number, number]

export const MAX_COORD = 10000
export const MAX_POINTS_PER_BATCH = 512
export const MAX_STROKE_TIME_MS = 600_000
export const MAX_GUESS_CHARS = 100
export const PING_INTERVAL_MS = 5_000

/** Part of a stroke. width is in the same normalized units as coordinates (0..10000). */
export type StrokeBatch = {
  stroke_id: string
  points: Point[]
  color: string
  width: number
  done: boolean
}

/** A stroke batch as the room stores and broadcasts it: with its author. */
export type CanvasBatch = StrokeBatch & { user_id: string }

export type PlayerView = {
  user_id: string
  name: string
  score: number
  connected: boolean
  guessed: boolean
}

export type ScoreLine = { user_id: string; name: string; score: number }

export type Result = ScoreLine & { placement: number }

/** A player's picture of the room, sent in welcome. It holds the word only after a round. */
export type View = {
  room_id: string
  phase: Phase
  host_id: string
  match_id?: string
  settings: Record<string, number>
  players: PlayerView[] | null
  round_no?: number
  drawer_ids?: string[]
  deadline?: number
  word_length?: number
  mask?: string
  word?: string
  strokes?: CanvasBatch[] | null
  /** The round's first correct viewer of a live room (ADR 0019). */
  viewer_winner?: ViewerWinner
}

export type ViewerWinner = { user_id: string; name: string }

/** A live room's most common wrong viewer guesses over the last 30 seconds (ADR 0018). */
export type TopGuesses = { round_no: number; window_end: number; guesses: { text: string; count: number }[] }

/** Shared room events: each has a seq and is stored in the room's event log. */
export type RoomEvent =
  | { seq: number; type: 'player_joined'; data: { user_id: string; name: string } }
  | { seq: number; type: 'player_left'; data: { user_id: string } }
  | { seq: number; type: 'player_removed'; data: { user_id: string } }
  | { seq: number; type: 'host_changed'; data: { user_id: string } }
  | { seq: number; type: 'match_started'; data: { match_id: string; order: string[]; deadline: number } }
  | { seq: number; type: 'choosing'; data: { round_no: number; drawer_ids: string[]; deadline: number } }
  | {
      seq: number
      type: 'round_started'
      data: { round_no: number; drawer_ids: string[]; word_length: number; deadline: number }
    }
  | { seq: number; type: 'stroke_batch'; data: CanvasBatch }
  | { seq: number; type: 'stroke_undone'; data: { stroke_id: string; user_id: string } }
  | { seq: number; type: 'hint'; data: { mask: string } }
  | { seq: number; type: 'chat'; data: { user_id: string; text: string } }
  | { seq: number; type: 'correct_guess'; data: { user_id: string; order: number } }
  | { seq: number; type: 'viewer_correct'; data: ViewerWinner & { round_no: number } }
  | {
      seq: number
      type: 'round_ended'
      data: { round_no: number; word: string; scores: ScoreLine[]; round_points: Record<string, number> }
    }
  | { seq: number; type: 'match_ended'; data: { match_id: string; results: Result[] } }

/** Private messages: no seq, never stored in the shared log. */
export type PrivateMessage =
  | { type: 'word_options'; data: { words: string[] } }
  | { type: 'your_word'; data: { word: string } }
  | { type: 'guess_result'; data: { correct: boolean } }
  | { type: 'rate_limited'; data: Record<string, never> }
  | { type: 'error'; data: { code: string; message: string } }
  // Viewers only (a relay, ADR 0019).
  | { type: 'top_guesses'; data: TopGuesses }

/** viewer is set on a relay's welcome: this socket watches a live room and can't play (ADR 0017). */
export type Welcome = { user_id: string; view: View; seq: number; viewer?: boolean }

export type ServerMessage =
  | { type: 'welcome'; data: Welcome }
  | { type: 'event'; data: RoomEvent }
  | { type: 'pong'; data: { t: number } }
  | { type: 'room_moved'; data: { reason: string } }
  // The last message of every resume (ADR 0008): the room's seq once the replay is done.
  | { type: 'resumed'; data: { seq: number } }
  | PrivateMessage

export type ClientMessage =
  | { type: 'hello'; data: { ticket: string; last_seq: number } }
  | { type: 'start'; data: Record<string, never> }
  | { type: 'choose_word'; data: { index: number } }
  | { type: 'stroke_batch'; data: StrokeBatch }
  | { type: 'undo'; data: { stroke_id: string } }
  | { type: 'guess'; data: { text: string } }
  | { type: 'ping'; data: { t: number } }

/** Close codes (design doc, section 6.4). */
export const CloseCode = {
  BadRequest: 4000,
  BadTicket: 4001,
  RoomFull: 4003,
  NoRoom: 4004,
  HelloTimeout: 4008,
  Replaced: 4009,
  /** The room isn't (or is no longer) on this server, usually after a takeover (ADR 0007).
   * Retryable on purpose: the lobby knows the room's new server. */
  RoomMoved: 4010,
} as const
