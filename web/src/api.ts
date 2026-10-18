// The JSON calls every REST client here makes: the access token, the shared error format
// (design doc, section 6.3), and one refresh-and-retry on 401 (auth.ts).

import { authFetch } from './auth.ts'

/** A failed call. code is the server's error.code, or SIGNED_OUT / HTTP_<status>. */
export class ApiError extends Error {
  readonly code: string
  readonly status: number

  constructor(code: string, status: number, message: string) {
    super(message)
    this.code = code
    this.status = status
  }
}

export async function call<T>(
  method: string,
  path: string,
  body?: unknown,
  extraHeaders: Record<string, string> = {},
): Promise<T> {
  const headers: Record<string, string> = { ...extraHeaders }
  const init: RequestInit = { method, headers }
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }
  const resp = await authFetch(path, init)
  if (!resp) throw new ApiError('SIGNED_OUT', 401, 'You are signed out.')
  if (!resp.ok) {
    const err = (await resp.json().catch(() => null)) as {
      error?: { code?: string; message?: string }
    } | null
    throw new ApiError(
      err?.error?.code ?? `HTTP_${resp.status}`,
      resp.status,
      err?.error?.message ?? `${method} ${path} failed with ${resp.status}`,
    )
  }
  if (resp.status === 204) return undefined as T
  return (await resp.json()) as T
}
