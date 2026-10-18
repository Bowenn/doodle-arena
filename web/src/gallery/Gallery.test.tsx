import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from '../auth.ts'
import type { Drawing } from '../gallery.ts'
import { Gallery } from './Gallery.tsx'

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function drawing(n: number, over: Partial<Drawing> = {}): Drawing {
  return {
    id: `00000000-0000-7000-8000-00000000000${n}`,
    match_id: 'm1',
    round_no: n,
    word: `word${n}`,
    author_ids: ['u_ada'],
    initial_owner_id: 'u_ada',
    status: 'approved',
    ended_at: '',
    created_at: '',
    has_replay: true,
    image_url: `http://s3/full${n}.png?sig`,
    thumb_url: `http://s3/thumb${n}.png?sig`,
    ...over,
  }
}

describe('Gallery', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  function serve(pages: Record<string, { drawings: Drawing[]; next_cursor: string | null }>) {
    fetchMock.mockImplementation((url: string) => {
      if (url === '/api/auth/refresh') {
        return Promise.resolve(json(200, { access_token: 't1', token_type: 'Bearer', expires_in: 900 }))
      }
      const page = pages[url]
      return Promise.resolve(page ? json(200, page) : json(500, { error: { code: 'INTERNAL', message: 'boom' } }))
    })
  }

  beforeEach(() => {
    resetAuthForTests()
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('shows approved drawings, opens one, and loads more', async () => {
    serve({
      '/api/assets/drawings?limit=24': { drawings: [drawing(2), drawing(1)], next_cursor: 'c1' },
      '/api/assets/drawings?cursor=c1&limit=24': { drawings: [drawing(3)], next_cursor: null },
    })
    const open = vi.fn()
    render(<Gallery meId="u_me" onOpen={open} onBack={() => {}} />)

    const thumbs = await screen.findAllByRole('img')
    expect(thumbs.map((i) => i.getAttribute('src'))).toEqual(['http://s3/thumb2.png?sig', 'http://s3/thumb1.png?sig'])
    fireEvent.click(screen.getByText('word2'))
    expect(open).toHaveBeenCalledWith(drawing(2).id)

    fireEvent.click(screen.getByRole('button', { name: 'Load more' }))
    expect(await screen.findByText('word3')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull()
  })

  it("lists my drawings with their status, from the author filter", async () => {
    serve({
      '/api/assets/drawings?limit=24': { drawings: [], next_cursor: null },
      '/api/assets/drawings?author_id=u_me&limit=24': {
        drawings: [drawing(1, { status: 'review', thumb_url: null, image_url: null })],
        next_cursor: null,
      },
    })
    render(<Gallery meId="u_me" onOpen={() => {}} onBack={() => {}} />)
    expect(await screen.findByText('No drawings yet.')).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Drawn by me' }))

    expect(await screen.findByText('Waiting for a moderator')).toBeTruthy()
    expect(screen.getByText('No image yet')).toBeTruthy()
  })

  it("shows one match's drawings in round order", async () => {
    serve({
      '/api/assets/drawings?match_id=m1&limit=24': { drawings: [drawing(3), drawing(1), drawing(2)], next_cursor: null },
    })
    render(<Gallery meId="u_me" matchId="m1" onOpen={() => {}} onBack={() => {}} />)

    await screen.findByText('word1')
    expect(screen.getAllByText(/^word\d$/).map((e) => e.textContent)).toEqual(['word1', 'word2', 'word3'])
    expect(screen.getByRole('heading').textContent).toBe("This match's drawings")
    expect(screen.queryByRole('button', { name: 'Drawn by me' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Refresh' })).toBeTruthy()
  })

  it('says so when the drawings cannot be loaded', async () => {
    serve({})
    render(<Gallery meId="u_me" onOpen={() => {}} onBack={() => {}} />)

    expect((await screen.findByRole('alert')).textContent).toBe('boom')
  })
})
