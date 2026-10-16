// Session handling for the web app (design doc 4.2, .claude/rules/web.md).
//
// The access token lives only in this module's memory. The refresh token is an HttpOnly cookie
// the page can't read; the browser sends it to /api/auth/refresh. The server rotates it on
// every use and revokes the whole family when a used token comes back, so two refreshes must
// never race: refresh() is single-flight.

export type Me = {
  id: string
  display_name: string
  avatar_url: string | null
  roles: string[]
}

// Refresh this long before the access token expires.
const REFRESH_MARGIN_SECONDS = 60

let accessToken: string | null = null
let inFlight: Promise<boolean> | null = null
let timer: ReturnType<typeof setTimeout> | null = null

/** Gets a new access token. Concurrent callers share one request. Resolves false when the
 * user is signed out. */
export function refresh(): Promise<boolean> {
  inFlight ??= doRefresh().finally(() => {
    inFlight = null
  })
  return inFlight
}

async function doRefresh(): Promise<boolean> {
  const resp = await fetch('/api/auth/refresh', { method: 'POST', credentials: 'same-origin' })
  if (!resp.ok) {
    clearSession()
    return false
  }
  const body = (await resp.json()) as { access_token: string; expires_in: number }
  accessToken = body.access_token
  scheduleRefresh(body.expires_in)
  return true
}

function scheduleRefresh(expiresIn: number) {
  if (timer) clearTimeout(timer)
  const delaySeconds = Math.max(expiresIn - REFRESH_MARGIN_SECONDS, 5)
  timer = setTimeout(() => void refresh(), delaySeconds * 1000)
}

function clearSession() {
  accessToken = null
  if (timer) clearTimeout(timer)
  timer = null
}

/** fetch with the access token. When the server answers 401 (the token expired), it refreshes
 * once and retries. Resolves null when the user is signed out. */
export async function authFetch(url: string, init: RequestInit = {}): Promise<Response | null> {
  for (let attempt = 0; attempt < 2; attempt++) {
    if (!accessToken && !(await refresh())) return null
    const headers = new Headers(init.headers)
    headers.set('Authorization', `Bearer ${accessToken}`)
    const resp = await fetch(url, { ...init, headers })
    if (resp.status !== 401) return resp
    accessToken = null
  }
  return null
}

/** The signed-in user, or null when signed out. Refreshes once if the token has expired. */
export async function fetchMe(): Promise<Me | null> {
  const resp = await authFetch('/api/auth/me')
  if (!resp) return null
  if (!resp.ok) throw new Error(`GET /api/auth/me failed with ${resp.status}`)
  return (await resp.json()) as Me
}

export async function logout(): Promise<void> {
  try {
    await fetch('/api/auth/logout', { method: 'POST', credentials: 'same-origin' })
  } finally {
    clearSession()
  }
}

/** Where "Sign in with Google" goes. Login is a full-page navigation through Google. */
export function loginUrl(returnTo = '/'): string {
  return `/api/auth/google/login?return_to=${encodeURIComponent(returnTo)}`
}

/** Test helper: forget the session and any pending request. */
export function resetAuthForTests() {
  clearSession()
  inFlight = null
}
