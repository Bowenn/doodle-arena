import { describe, expect, it } from 'vitest'
import type { RoomEvent, View, Welcome } from './protocol.ts'
import { canStart, isDrawer, roomReducer, wordLine, type RoomState } from './roomState.ts'

function view(over: Partial<View> = {}): View {
  return {
    room_id: 'r1',
    phase: 'waiting',
    host_id: 'a',
    settings: { max_players: 8 },
    players: [
      { user_id: 'a', name: 'Ada', score: 0, connected: true, guessed: false },
      { user_id: 'b', name: 'Bob', score: 0, connected: true, guessed: false },
    ],
    ...over,
  }
}

function welcome(seq: number, over: Partial<View> = {}, me = 'a'): Welcome {
  return { user_id: me, view: view(over), seq }
}

function start(w: Welcome = welcome(5)): RoomState {
  return roomReducer(null, { type: 'welcome', welcome: w })!
}

function apply(s: RoomState, ...events: RoomEvent[]): RoomState {
  return events.reduce((acc, event) => roomReducer(acc, { type: 'event', event })!, s)
}

describe('roomReducer', () => {
  it('ignores events until a welcome arrives', () => {
    expect(
      roomReducer(null, { type: 'event', event: { seq: 1, type: 'host_changed', data: { user_id: 'b' } } }),
    ).toBeNull()
  })

  it('applies only events newer than the welcome, in seq order', () => {
    let s = start(welcome(5))
    s = apply(
      s,
      { seq: 4, type: 'host_changed', data: { user_id: 'old' } }, // already in the view
      { seq: 5, type: 'host_changed', data: { user_id: 'old' } }, // already in the view
      { seq: 6, type: 'host_changed', data: { user_id: 'b' } },
      { seq: 6, type: 'host_changed', data: { user_id: 'dup' } }, // a duplicate
    )
    expect(s.hostId).toBe('b')
    expect(s.seq).toBe(6)
  })

  it('replaces the state on a new welcome but keeps the chat feed', () => {
    let s = start(welcome(1))
    s = apply(s, { seq: 2, type: 'chat', data: { user_id: 'b', text: 'hi' } })
    s = apply(s, { seq: 3, type: 'stroke_batch', data: batch('s1', 'b') })

    s = roomReducer(s, { type: 'welcome', welcome: welcome(10, { host_id: 'b' }) })!

    expect(s.seq).toBe(10)
    expect(s.hostId).toBe('b')
    expect(s.strokes).toEqual([]) // the view had no strokes
    expect(s.feed).toContainEqual({ kind: 'chat', user_id: 'b', text: 'hi' })
  })

  it('takes the canvas from the welcome view', () => {
    const s = start(welcome(3, { phase: 'drawing', strokes: [batch('s1', 'b'), batch('s1', 'b')] }))
    expect(s.strokes).toHaveLength(2)
  })

  it('removes every batch of an undone stroke, but only the author’s', () => {
    let s = start()
    s = apply(
      s,
      { seq: 6, type: 'stroke_batch', data: batch('s1', 'a') },
      { seq: 7, type: 'stroke_batch', data: batch('s1', 'a') },
      { seq: 8, type: 'stroke_batch', data: batch('s1', 'b') },
      { seq: 9, type: 'stroke_batch', data: batch('s2', 'a') },
      { seq: 10, type: 'stroke_undone', data: { stroke_id: 's1', user_id: 'a' } },
    )
    expect(s.strokes.map((b) => `${b.user_id}:${b.stroke_id}`)).toEqual(['b:s1', 'a:s2'])
  })

  it('follows a round: choosing, drawing, hints, a correct guess, the reveal', () => {
    let s = start(welcome(5, {}, 'b'))
    s = apply(s, { seq: 6, type: 'match_started', data: { match_id: 'm1', order: ['a', 'b'], deadline: 1000 } })
    expect(s.phase).toBe('starting')

    s = apply(s, { seq: 7, type: 'choosing', data: { round_no: 1, drawer_ids: ['a'], deadline: 2000 } })
    expect(s.phase).toBe('choosing')
    expect(isDrawer(s)).toBe(false)

    s = apply(s, { seq: 8, type: 'round_started', data: { round_no: 1, drawer_ids: ['a'], word_length: 5, deadline: 3000 } })
    expect(s.phase).toBe('drawing')
    expect(wordLine(s)).toBe('_____')
    expect(s.deadline).toBe(3000)

    s = apply(s, { seq: 9, type: 'hint', data: { mask: 'a____' } })
    expect(wordLine(s)).toBe('a____')

    s = apply(s, { seq: 10, type: 'correct_guess', data: { user_id: 'b', order: 1 } })
    expect(s.players.find((p) => p.user_id === 'b')?.guessed).toBe(true)
    // Scores change only when the server says so.
    expect(s.players.find((p) => p.user_id === 'b')?.score).toBe(0)

    s = apply(s, {
      seq: 11,
      type: 'round_ended',
      data: {
        round_no: 1,
        word: 'apple',
        scores: [
          { user_id: 'b', name: 'Bob', score: 100 },
          { user_id: 'a', name: 'Ada', score: 10 },
        ],
        round_points: { b: 100, a: 10 },
      },
    })
    expect(s.phase).toBe('reveal')
    expect(wordLine(s)).toBe('apple')
    expect(s.players.find((p) => p.user_id === 'b')?.score).toBe(100)
    expect(s.deadline).toBeNull()

    s = apply(s, { seq: 12, type: 'choosing', data: { round_no: 2, drawer_ids: ['b'], deadline: 4000 } })
    expect(s.word).toBeNull()
    expect(s.players.every((p) => !p.guessed)).toBe(true)
    expect(isDrawer(s)).toBe(true)
  })

  it('shows the drawer their word and options only from private messages', () => {
    let s = start(welcome(5, { phase: 'choosing', drawer_ids: ['a'], round_no: 1 }))
    expect(s.wordOptions).toBeNull()
    s = roomReducer(s, { type: 'private', msg: { type: 'word_options', data: { words: ['cat', 'dog', 'sun'] } } })!
    expect(s.wordOptions).toEqual(['cat', 'dog', 'sun'])

    s = apply(s, { seq: 6, type: 'round_started', data: { round_no: 1, drawer_ids: ['a'], word_length: 3, deadline: 1 } })
    expect(s.wordOptions).toBeNull()
    expect(wordLine(s)).toBe('___')
    s = roomReducer(s, { type: 'private', msg: { type: 'your_word', data: { word: 'dog' } } })!
    expect(wordLine(s)).toBe('dog')
  })

  it('tracks joins, leaves, removals and the host', () => {
    let s = start()
    s = apply(
      s,
      { seq: 6, type: 'player_joined', data: { user_id: 'c', name: 'Cy' } },
      { seq: 7, type: 'player_left', data: { user_id: 'a' } },
      { seq: 8, type: 'host_changed', data: { user_id: 'b' } },
      { seq: 9, type: 'player_joined', data: { user_id: 'a', name: 'Ada L' } },
      { seq: 10, type: 'player_removed', data: { user_id: 'c' } },
    )
    expect(s.players.map((p) => [p.user_id, p.name, p.connected])).toEqual([
      ['a', 'Ada L', true],
      ['b', 'Bob', true],
    ])
    expect(s.hostId).toBe('b')
  })

  it('ends a match with the server’s results', () => {
    let s = start(welcome(5, { phase: 'reveal' }))
    s = apply(s, {
      seq: 6,
      type: 'match_ended',
      data: {
        match_id: 'm1',
        results: [
          { user_id: 'a', name: 'Ada', score: 90, placement: 1 },
          { user_id: 'b', name: 'Bob', score: 90, placement: 1 },
        ],
      },
    })
    expect(s.phase).toBe('match_end')
    expect(s.results?.map((r) => r.placement)).toEqual([1, 1])
    expect(canStart(s)).toBe(true) // a is host and both are connected
  })

  it('offers Start only to the host with two connected players', () => {
    expect(canStart(start())).toBe(true)
    expect(canStart(start(welcome(1, {}, 'b')))).toBe(false)
    let s = start()
    s = apply(s, { seq: 6, type: 'player_left', data: { user_id: 'b' } })
    expect(canStart(s)).toBe(false)
  })

  it('turns private feedback into notices', () => {
    let s = start()
    s = roomReducer(s, { type: 'private', msg: { type: 'rate_limited', data: {} } })!
    expect(s.notice?.tone).toBe('bad')
    s = roomReducer(s, { type: 'private', msg: { type: 'guess_result', data: { correct: true } } })!
    expect(s.notice?.tone).toBe('good')
  })
})

function batch(strokeId: string, userId: string) {
  return { stroke_id: strokeId, user_id: userId, points: [[1, 2, 0]] as [number, number, number][], color: '#000000', width: 40, done: false }
}
