import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from '../auth.ts'
import type { Replay } from '../gallery.ts'
import { DrawingPage } from './DrawingPage.tsx'

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const ID = '01a14d4b-278f-74a1-a0be-62cc85301740'
const DRAWING = {
  id: ID,
  match_id: 'm1',
  round_no: 2,
  word: 'cat',
  author_ids: ['u_me', 'u_bob12345678'],
  initial_owner_id: 'u_me',
  status: 'approved',
  ended_at: '',
  created_at: '',
  has_replay: true,
  image_url: 'http://s3/full.png?sig',
  thumb_url: 'http://s3/thumb.png?sig',
}
// Two strokes; the replay lasts 1.5 s at 1x.
const REPLAY: Replay = {
  drawing_id: ID,
  schema_version: 1,
  canvas_width: 800,
  canvas_height: 600,
  round_started_at: null,
  strokes: [
    { user_id: 'u_me', color: '#000000', width: 60, undone: false, server_received_ms: 1000, points: [[0, 0, 0], [5000, 5000, 1000]] },
    { user_id: 'u_bob12345678', color: '#e03131', width: 60, undone: false, server_received_ms: 1500, points: [[9000, 9000, 0]] },
  ],
}

describe('DrawingPage', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    resetAuthForTests()
    fetchMock = vi.fn((url: string) => {
      if (url === '/api/auth/refresh') {
        return Promise.resolve(json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 }))
      }
      if (url === `/api/assets/drawings/${ID}`) return Promise.resolve(json(200, DRAWING))
      if (url === `/api/assets/drawings/${ID}/replay`) return Promise.resolve(json(200, REPLAY))
      return Promise.resolve(json(404, { error: { code: 'DRAWING_NOT_FOUND', message: 'no such drawing' } }))
    })
    vi.stubGlobal('fetch', fetchMock)
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue(null)
    // Fake timers from the start (the replay autoplays), still moving with real time so the
    // async lookups resolve.
    vi.useFakeTimers({ shouldAdvanceTime: true })
  })

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
    vi.useRealTimers()
  })

  it('shows the drawing and plays its replay to the end', async () => {
    render(<DrawingPage drawingId={ID} meId="u_me" onBack={() => {}} />)

    expect(await screen.findByText('“cat”')).toBeTruthy()
    expect(screen.getByText(/drawn by you and Player u_bob123/)).toBeTruthy()
    const clock = await screen.findByLabelText('Replay position')
    expect(clock.textContent).toMatch(/ s \/ 1\.5 s$/)

    fireEvent.click(screen.getByRole('button', { name: 'Restart' }))
    act(() => vi.advanceTimersByTime(2000))

    expect(clock.textContent).toBe('1.5 s / 1.5 s')
    expect(screen.getByRole('button', { name: 'Play' })).toBeTruthy() // stopped at the end
  })

  it('plays faster at 4x', async () => {
    render(<DrawingPage drawingId={ID} meId="u_me" onBack={() => {}} />)
    const clock = await screen.findByLabelText('Replay position')

    fireEvent.click(screen.getByRole('button', { name: '4×' }))
    fireEvent.click(screen.getByRole('button', { name: 'Restart' }))
    act(() => vi.advanceTimersByTime(400))

    expect(clock.textContent).toBe('1.5 s / 1.5 s')
    expect(screen.getByRole('button', { name: '4×' }).getAttribute('aria-pressed')).toBe('true')
  })

  it('pauses and plays again from the start once finished', async () => {
    render(<DrawingPage drawingId={ID} meId="u_me" onBack={() => {}} />)
    const clock = await screen.findByLabelText('Replay position')
    fireEvent.click(screen.getByRole('button', { name: 'Restart' }))
    act(() => vi.advanceTimersByTime(500))
    fireEvent.click(screen.getByRole('button', { name: 'Pause' }))
    const paused = clock.textContent
    act(() => vi.advanceTimersByTime(1000))
    expect(clock.textContent).toBe(paused)

    fireEvent.click(screen.getByRole('button', { name: 'Play' }))
    act(() => vi.advanceTimersByTime(2000))
    fireEvent.click(screen.getByRole('button', { name: 'Play' })) // finished: starts over
    expect(clock.textContent).toBe('0.0 s / 1.5 s')
  })

  it("says the drawing isn't available on a 404", async () => {
    render(<DrawingPage drawingId="00000000-0000-7000-8000-000000000000" meId="u_me" onBack={() => {}} />)

    expect((await screen.findByRole('alert')).textContent).toBe("This drawing isn't available.")
  })
})
