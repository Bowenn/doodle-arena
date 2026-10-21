import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from '../auth.ts'
import type { View } from '../game/protocol.ts'
import { Room } from './Room.tsx'

class FakeSocket {
  static readonly OPEN = 1
  static all: FakeSocket[] = []
  readonly url: string
  readyState = 0
  sent: { type: string; data: unknown }[] = []
  onopen: (() => void) | null = null
  onmessage: ((ev: { data: string }) => void) | null = null
  onclose: ((ev: { code: number }) => void) | null = null

  constructor(url: string) {
    this.url = url
    FakeSocket.all.push(this)
  }

  send(data: string) {
    this.sent.push(JSON.parse(data))
  }

  close() {}

  open() {
    this.readyState = FakeSocket.OPEN
    act(() => this.onopen?.())
  }

  receive(type: string, data: unknown) {
    act(() => this.onmessage?.({ data: JSON.stringify({ type, data }) }))
  }

  event(seq: number, type: string, data: unknown) {
    this.receive('event', { seq, type, data })
  }
}

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const PLAYERS = [
  { user_id: 'a', name: 'Ada', score: 0, connected: true, guessed: false },
  { user_id: 'b', name: 'Bob', score: 0, connected: true, guessed: false },
]

function view(over: Partial<View> = {}): View {
  return { room_id: 'r1', phase: 'waiting', host_id: 'a', settings: {}, players: PLAYERS, ...over }
}

async function joined(
  me: string,
  v: View = view(),
  seq = 2,
  onMatchDrawings?: (matchId: string) => void,
): Promise<FakeSocket> {
  render(<Room roomId="r1" onLeave={() => {}} onMatchDrawings={onMatchDrawings} />)
  await vi.waitFor(() => expect(FakeSocket.all).toHaveLength(1))
  const ws = FakeSocket.all[0]
  ws.open()
  ws.receive('welcome', { user_id: me, view: v, seq })
  return ws
}

const sentOf = (ws: FakeSocket, type: string) => ws.sent.filter((m) => m.type === type).map((m) => m.data)

describe('Room', () => {
  beforeEach(() => {
    resetAuthForTests()
    FakeSocket.all = []
    vi.stubGlobal('WebSocket', FakeSocket)
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) =>
        Promise.resolve(
          url === '/api/auth/refresh'
            ? json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 })
            : json(200, { room_id: 'r1', ws_url: '/ws/gs-1/rooms/r1', ticket: 'tk', expires_at: '' }),
        ),
      ),
    )
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue(null)
  })

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('joins with a ticket from the lobby and lets the host start', async () => {
    const ws = await joined('a')
    expect(ws.url).toBe('ws://localhost:3000/ws/gs-1/rooms/r1')
    expect(ws.sent[0]).toEqual({ type: 'hello', data: { ticket: 'tk', last_seq: 0 } })
    expect(screen.getByText('Ada (you)')).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Start the match' }))
    expect(sentOf(ws, 'start')).toEqual([{}])
  })

  it('shows the drawer the word options and sends the choice', async () => {
    const ws = await joined('a')
    ws.event(3, 'choosing', { round_no: 1, drawer_ids: ['a'], deadline: Date.now() + 10_000 })
    ws.receive('word_options', { words: ['cat', 'dog', 'sun'] })

    fireEvent.click(screen.getByRole('button', { name: 'dog' }))
    expect(sentOf(ws, 'choose_word')).toEqual([{ index: 1 }])
  })

  it('lets the drawer draw and sends the stroke in batches', async () => {
    vi.spyOn(HTMLCanvasElement.prototype, 'getBoundingClientRect').mockReturnValue(
      { left: 0, top: 0, width: 400, height: 300, right: 400, bottom: 300, x: 0, y: 0, toJSON() {} },
    )
    HTMLCanvasElement.prototype.setPointerCapture = () => {}
    const ws = await joined('a', view({ phase: 'drawing', drawer_ids: ['a'], round_no: 1, word_length: 3 }))
    ws.receive('your_word', { word: 'dog' })
    expect(screen.getByLabelText('Word').textContent).toBe('d o g')
    expect(screen.getByLabelText<HTMLInputElement>('Guess').disabled).toBe(true)

    const surface = document.querySelector('canvas.overlay')!
    fireEvent.pointerDown(surface, { clientX: 100, clientY: 150, pointerId: 1, button: 0 })
    fireEvent.pointerMove(surface, { clientX: 200, clientY: 150, pointerId: 1 })
    fireEvent.pointerUp(surface, { pointerId: 1 })

    const [batch] = sentOf(ws, 'stroke_batch') as { points: number[][]; done: boolean; color: string; width: number }[]
    expect(batch.points.map(([x, y]) => [x, y])).toEqual([[2500, 5000], [5000, 5000]])
    expect(batch.done).toBe(true)
    expect(batch.color).toBe('#000000')
    expect(batch.width).toBe(60)
  })

  it('lets the second of two drawers wait for the choice, then draw and undo only their own strokes', async () => {
    const four = [
      ...PLAYERS,
      { user_id: 'c', name: 'Cy', score: 0, connected: true, guessed: false },
      { user_id: 'd', name: 'Di', score: 0, connected: true, guessed: false },
    ]
    const ws = await joined('b', view({ players: four }))
    ws.event(3, 'choosing', { round_no: 1, drawer_ids: ['a', 'b'], deadline: Date.now() + 10_000 })
    expect(screen.getByText('Ada is choosing a word. You draw it together.')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'dog' })).toBeNull()
    expect(screen.getByRole<HTMLInputElement>('textbox', { name: 'Chat' }).disabled).toBe(true)

    ws.event(4, 'round_started', { round_no: 1, drawer_ids: ['a', 'b'], word_length: 3, deadline: Date.now() + 80_000 })
    ws.receive('your_word', { word: 'dog' })
    expect(screen.getByText('You and Ada are drawing')).toBeTruthy()
    expect(screen.getByLabelText('Word').textContent).toBe('d o g')

    const line = { points: [[1, 2, 0]], color: '#000000', width: 40, done: true }
    ws.event(5, 'stroke_batch', { user_id: 'b', stroke_id: 's1', ...line })
    ws.event(6, 'stroke_batch', { user_id: 'a', stroke_id: 's1', ...line }) // Ada's is newer
    fireEvent.click(screen.getByRole('button', { name: 'Undo' }))
    expect(sentOf(ws, 'undo')).toEqual([{ stroke_id: 's1' }])
    ws.event(7, 'stroke_batch', { user_id: 'a', stroke_id: 's2', ...line })
    fireEvent.click(screen.getByRole('button', { name: 'Undo' }))
    expect(sentOf(ws, 'undo')).toEqual([{ stroke_id: 's1' }, { stroke_id: 's1' }]) // never Ada's s2
  })

  it('sends guesses and shows what the server decides', async () => {
    const ws = await joined('b', view({ phase: 'drawing', drawer_ids: ['a'], round_no: 1, word_length: 5, mask: '_____', deadline: Date.now() + 30_000 }))
    expect(screen.getByLabelText('Word').textContent).toBe('_ _ _ _ _')
    expect(screen.getByLabelText('Time left').textContent).toMatch(/^(29|30)s$/)
    expect(screen.queryByRole('toolbar')).toBeNull() // guessers can't draw

    const box = screen.getByLabelText<HTMLInputElement>('Guess')
    fireEvent.change(box, { target: { value: '  apple ' } })
    fireEvent.submit(box.form!)
    expect(sentOf(ws, 'guess')).toEqual([{ text: 'apple' }])
    expect(box.value).toBe('')

    ws.event(3, 'correct_guess', { user_id: 'b', order: 1 })
    ws.receive('guess_result', { correct: true })
    expect(screen.getByText('Bob guessed the word!')).toBeTruthy()
    expect(screen.getByText('You guessed it!')).toBeTruthy()

    ws.event(4, 'round_ended', {
      round_no: 1,
      word: 'apple',
      scores: [{ user_id: 'b', name: 'Bob', score: 100 }, { user_id: 'a', name: 'Ada', score: 10 }],
      round_points: { b: 100, a: 10 },
    })
    expect(screen.getByText('apple', { selector: 'strong' })).toBeTruthy()
    expect(screen.getByText('Bob +100, Ada +10')).toBeTruthy()
  })

  it('does not claim nobody scored after rejoining during the reveal', async () => {
    await joined('b', view({ phase: 'reveal', round_no: 1, word: 'apple' }))
    expect(screen.getByText('apple', { selector: 'strong' })).toBeTruthy()
    expect(screen.queryByText('Nobody guessed it.')).toBeNull()
  })

  it('takes the drawing tools away when the connection ends', async () => {
    const ws = await joined('a', view({ phase: 'drawing', drawer_ids: ['a'], round_no: 1, word_length: 3 }))
    expect(screen.getByRole('toolbar')).toBeTruthy()
    act(() => ws.onclose?.({ code: 4004 }))
    expect(screen.queryByRole('toolbar')).toBeNull()
  })

  it('shows why the connection ended', async () => {
    const ws = await joined('b')
    act(() => ws.onclose?.({ code: 4003 }))
    expect(screen.getByRole('alert').textContent).toBe('This room is full.')
    expect(screen.getByRole<HTMLInputElement>('textbox', { name: 'Chat' }).disabled).toBe(true)
  })
  it("links to the match's drawings once the match has ended", async () => {
    const open = vi.fn()
    const ws = await joined('a', view({ phase: 'drawing', match_id: 'm1' }), 2, open)
    expect(screen.queryByRole('button', { name: "See this match's drawings" })).toBeNull()

    ws.event(3, 'match_ended', {
      match_id: 'm1',
      results: [
        { user_id: 'a', name: 'Ada', score: 120, placement: 1 },
        { user_id: 'b', name: 'Bob', score: 80, placement: 2 },
      ],
    })
    fireEvent.click(screen.getByRole('button', { name: "See this match's drawings" }))

    expect(open).toHaveBeenCalledWith('m1')
  })

  it('watches a live room through a relay: nothing to start, draw or guess (ADR 0017)', async () => {
    const fetchMock = vi.fn((url: string) =>
      Promise.resolve(
        url === '/api/auth/refresh'
          ? json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 })
          : json(200, { room_id: 'r1', ws_url: '/ws/relay-1/rooms/r1', ticket: 'vt', expires_at: '' }),
      ),
    )
    vi.stubGlobal('fetch', fetchMock)
    vi.spyOn(HTMLCanvasElement.prototype, 'getBoundingClientRect').mockReturnValue(
      { left: 0, top: 0, width: 400, height: 300, right: 400, bottom: 300, x: 0, y: 0, toJSON() {} },
    )
    HTMLCanvasElement.prototype.setPointerCapture = () => {}
    render(<Room roomId="r1" onLeave={() => {}} watch />)
    await vi.waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    const ws = FakeSocket.all[0]
    expect(fetchMock.mock.calls.some(([url]) => url === '/api/lobby/rooms/r1/watch')).toBe(true)
    expect(ws.url).toBe('ws://localhost:3000/ws/relay-1/rooms/r1')
    ws.open()
    expect(ws.sent[0]).toEqual({ type: 'hello', data: { ticket: 'vt', last_seq: 0 } })
    ws.receive('welcome', {
      user_id: 'v', seq: 5, viewer: true,
      view: view({ phase: 'drawing', drawer_ids: ['a'], round_no: 1, word_length: 3, mask: '___' }),
    })

    expect(screen.getByRole('heading', { name: 'Watching room r1' })).toBeTruthy()
    expect(screen.queryByLabelText('Guess')).toBeNull()
    expect(screen.getByText("You are watching. Viewers can't guess yet.")).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Start the match' })).toBeNull()

    // The canvas is read-only, and the room's strokes still arrive.
    const surface = document.querySelector('canvas.overlay')
    if (surface) {
      fireEvent.pointerDown(surface, { clientX: 100, clientY: 150, pointerId: 1, button: 0 })
      fireEvent.pointerUp(surface, { pointerId: 1 })
    }
    ws.event(6, 'stroke_batch', { user_id: 'a', stroke_id: 's1', points: [[1, 1, 0]], color: '#000000', width: 5, done: true })
    expect(ws.sent.filter((m) => m.type !== 'hello' && m.type !== 'ping')).toEqual([])
  })
})
