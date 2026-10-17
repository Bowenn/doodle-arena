import { useEffect, useState } from 'react'
import { type AdminSignIn, adminNext, signInToAdmin } from './moderation.ts'

// Module-level, so the effect below sees the same function on every render.
const openPage = (url: string) => window.location.assign(url)

/** /moderate: the Django admin's sign-in page. It opens the admin, or says why it can't. */
export function Moderate({ onBack, navigate = openPage }: {
  onBack: () => void
  navigate?: (url: string) => void
}) {
  const [state, setState] = useState<AdminSignIn | { kind: 'working' }>({ kind: 'working' })

  useEffect(() => {
    let cancelled = false
    signInToAdmin(adminNext(window.location.search))
      .then((result) => {
        if (cancelled) return
        if (result.kind === 'ok') navigate(result.next)
        else setState(result)
      })
      .catch(() => {
        if (!cancelled) setState({ kind: 'error' })
      })
    return () => {
      cancelled = true
    }
  }, [navigate])

  return (
    <main>
      <h2>Moderation</h2>
      {state.kind === 'working' && <p>Opening the moderation console…</p>}
      {state.kind === 'forbidden' && <p role="alert">Your account doesn't have the moderator role.</p>}
      {state.kind === 'signed-out' && <p role="alert">You were signed out. Sign in again to moderate.</p>}
      {state.kind === 'error' && <p role="alert">Could not open the moderation console. Try again shortly.</p>}
      {state.kind !== 'working' && (
        <button type="button" onClick={onBack}>
          Back to the lobby
        </button>
      )}
    </main>
  )
}
