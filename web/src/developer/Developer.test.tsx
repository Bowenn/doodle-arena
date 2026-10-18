import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetAuthForTests } from '../auth.ts'
import type { ApiKey, Delivery, Endpoint } from '../developer.ts'
import { json, mockApi, type Route } from '../market/testing.ts'
import { Developer } from './Developer.tsx'

const KEY: ApiKey = {
  id: 'k1',
  name: 'old script',
  prefix: 'abcdefabcdef',
  scopes: ['webhooks'],
  created_at: '2026-10-18T10:00:00Z',
  last_used_at: null,
}
const EP: Endpoint = {
  id: 'e1',
  url: 'https://example.com/hook',
  event_types: ['coins.credited'],
  created_at: '2026-10-18T10:00:00Z',
}
const DEAD: Delivery = {
  id: 'd1',
  event_id: 'ev1',
  event_type: 'coins.credited',
  status: 'DEAD',
  attempts: 8,
  last_status_code: 500,
  last_error: null,
  next_attempt_at: null,
  created_at: '2026-10-18T10:00:00Z',
  finished_at: '2026-10-18T10:05:00Z',
  attempt_log: [],
}

describe('Developer', () => {
  let routes: Record<string, Route>

  beforeEach(() => {
    resetAuthForTests()
    routes = {
      'GET /api/auth/api-keys': () => json(200, { api_keys: [KEY] }),
      'GET /api/notifier/endpoints': () => json(200, { endpoints: [EP] }),
    }
  })

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('shows a new API key once, and revokes keys', async () => {
    let body: unknown
    routes['POST /api/auth/api-keys'] = (init) => {
      body = JSON.parse(String(init.body))
      return json(201, { ...KEY, id: 'k2', name: 'deploy bot', prefix: '123456123456', key: 'da_123456123456_s3cret' })
    }
    routes['DELETE /api/auth/api-keys/k1'] = () => new Response(null, { status: 204 })
    mockApi(routes)
    render(<Developer />)

    expect(await screen.findByText('old script')).toBeTruthy()
    fireEvent.change(screen.getByLabelText('Key name'), { target: { value: 'deploy bot' } })
    fireEvent.click(screen.getByRole('button', { name: 'Create key' }))

    expect(await screen.findByText('da_123456123456_s3cret')).toBeTruthy()
    expect(body).toEqual({ name: 'deploy bot', scopes: ['webhooks'] })
    expect(screen.getByText('deploy bot')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Done' }))
    expect(screen.queryByText('da_123456123456_s3cret')).toBeNull()

    const old = screen.getByText('old script').closest('li')!
    fireEvent.click(within(old).getByRole('button', { name: 'Revoke' }))
    await vi.waitFor(() => expect(screen.queryByText('old script')).toBeNull())
  })

  it('registers an endpoint and shows its secret once', async () => {
    let body: unknown
    routes['POST /api/notifier/endpoints'] = (init) => {
      body = JSON.parse(String(init.body))
      return json(201, { ...EP, id: 'e2', url: 'https://example.org/in', event_types: ['sale.completed'], secret: 'whsec_abc' })
    }
    mockApi(routes)
    render(<Developer />)

    fireEvent.change(await screen.findByLabelText('Endpoint URL'), { target: { value: 'https://example.org/in' } })
    fireEvent.click(screen.getByLabelText('coins.credited'))
    fireEvent.click(screen.getByRole('button', { name: 'Register endpoint' }))

    expect(await screen.findByText('whsec_abc')).toBeTruthy()
    expect(body).toEqual({ url: 'https://example.org/in', event_types: ['sale.completed'] })
    expect(screen.getByText('https://example.org/in')).toBeTruthy()
  })

  it('explains a refused URL', async () => {
    routes['POST /api/notifier/endpoints'] = () =>
      json(400, { error: { code: 'INVALID_URL', message: 'the URL must use https' } })
    mockApi(routes)
    render(<Developer />)

    fireEvent.change(await screen.findByLabelText('Endpoint URL'), { target: { value: 'http://10.0.0.1/' } })
    fireEvent.click(screen.getByRole('button', { name: 'Register endpoint' }))
    expect(await screen.findByText('the URL must use https')).toBeTruthy()
  })

  it('sends a test event and shows the delivery log', async () => {
    let tested = false
    routes['POST /api/notifier/endpoints/e1/test'] = () => {
      tested = true
      return json(202, { delivery_id: 'd2', event_id: 'ping_1' })
    }
    routes['GET /api/notifier/endpoints/e1/deliveries?limit=20'] = () =>
      json(200, {
        deliveries: [
          { ...DEAD, id: 'd2', event_type: 'ping', status: 'PENDING', attempts: 0, last_status_code: null },
          DEAD,
        ],
        next_cursor: null,
      })
    routes['DELETE /api/notifier/endpoints/e1'] = () => new Response(null, { status: 204 })
    mockApi(routes)
    render(<Developer />)

    fireEvent.click(await screen.findByRole('button', { name: 'Send test' }))
    expect(await screen.findByText('Test event queued.')).toBeTruthy()
    expect(tested).toBe(true)
    expect(await screen.findByText('DEAD')).toBeTruthy()
    expect(screen.getByText('PENDING')).toBeTruthy()
    expect(screen.getByText('HTTP 500')).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
    await vi.waitFor(() => expect(screen.queryByText('https://example.com/hook')).toBeNull())
  })
})
