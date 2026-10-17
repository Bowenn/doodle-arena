import { authFetch } from './auth.ts'

export type AdminSignIn = { kind: 'ok'; next: string } | { kind: 'forbidden' } | { kind: 'signed-out' } | { kind: 'error' }

/** The admin page to open afterwards: only admin pages (assets checks this too). */
export function adminNext(search: string): string {
  const next = new URLSearchParams(search).get('next')
  return next?.startsWith('/admin/') ? next : '/admin/'
}

/** Signs in to the Django admin (ADR 0001): asks assets for a CSRF token, then hands it the
 * access token once, in the Authorization header. assets answers with an admin session cookie
 * scoped to /admin/. */
export async function signInToAdmin(next: string): Promise<AdminSignIn> {
  const csrf = await fetch('/admin/sso/', { credentials: 'same-origin' })
  if (!csrf.ok) return { kind: 'error' }
  const { csrf_token } = (await csrf.json()) as { csrf_token: string }
  const resp = await authFetch(`/admin/sso/?next=${encodeURIComponent(next)}`, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'X-CSRFToken': csrf_token },
  })
  if (!resp) return { kind: 'signed-out' }
  if (resp.ok) return { kind: 'ok', next: ((await resp.json()) as { next: string }).next }
  // A CSRF failure is a 403 too, but an HTML one; only the JSON error means a missing role.
  const body = (await resp.json().catch(() => null)) as { error?: { code?: string } } | null
  return body?.error?.code === 'FORBIDDEN' ? { kind: 'forbidden' } : { kind: 'error' }
}
