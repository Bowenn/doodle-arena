// Developer settings clients: API keys (auth) and webhook endpoints (notifier), design doc 4.7.
// Both accept the session's access token; API keys are for scripts.

import { call } from './api.ts'

export type ApiKey = {
  id: string
  name: string
  prefix: string
  scopes: string[]
  created_at: string
  last_used_at: string | null
}

/** Only the create response carries the key itself; it is never shown again. */
export type CreatedApiKey = ApiKey & { key: string }

export const EVENT_TYPES = ['sale.completed', 'coins.credited'] as const

export type Endpoint = {
  id: string
  url: string
  event_types: string[]
  created_at: string
  /** disabled: it receives nothing until enabled (credentials_revoked: auth revoked the
   * owner's sign-in after a stolen session was detected). */
  status: 'active' | 'disabled'
  disabled_reason: string | null
}

/** Only the register response carries the signing secret; it is never shown again. */
export type CreatedEndpoint = Endpoint & { secret: string }

export type DeliveryStatus = 'PENDING' | 'SUCCEEDED' | 'DEAD' | 'CANCELLED'

export type Delivery = {
  id: string
  event_id: string
  event_type: string
  status: DeliveryStatus
  attempts: number
  last_status_code: number | null
  last_error: string | null
  next_attempt_at: string | null
  created_at: string
  finished_at: string | null
  attempt_log: { attempt: number; status_code: number; error: string | null; duration_ms: number; at: string }[]
}

export async function listApiKeys(): Promise<ApiKey[]> {
  return (await call<{ api_keys: ApiKey[] }>('GET', '/api/auth/api-keys')).api_keys
}

export function createApiKey(name: string): Promise<CreatedApiKey> {
  return call('POST', '/api/auth/api-keys', { name, scopes: ['webhooks'] })
}

export function revokeApiKey(id: string): Promise<void> {
  return call('DELETE', `/api/auth/api-keys/${encodeURIComponent(id)}`)
}

export async function listEndpoints(): Promise<Endpoint[]> {
  return (await call<{ endpoints: Endpoint[] }>('GET', '/api/notifier/endpoints')).endpoints
}

export function createEndpoint(url: string, eventTypes: string[]): Promise<CreatedEndpoint> {
  return call('POST', '/api/notifier/endpoints', { url, event_types: eventTypes })
}

/** Makes a disabled endpoint live again with a new signing secret, shown only in this answer. */
export function enableEndpoint(id: string): Promise<CreatedEndpoint> {
  return call('POST', `/api/notifier/endpoints/${encodeURIComponent(id)}/enable`)
}

export function deleteEndpoint(id: string): Promise<void> {
  return call('DELETE', `/api/notifier/endpoints/${encodeURIComponent(id)}`)
}

export async function listDeliveries(endpointId: string): Promise<Delivery[]> {
  const page = await call<{ deliveries: Delivery[] }>(
    'GET',
    `/api/notifier/endpoints/${encodeURIComponent(endpointId)}/deliveries?limit=20`,
  )
  return page.deliveries
}

export function sendTest(endpointId: string): Promise<{ delivery_id: string; event_id: string }> {
  return call('POST', `/api/notifier/endpoints/${encodeURIComponent(endpointId)}/test`)
}
