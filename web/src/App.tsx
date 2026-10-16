import { useEffect, useState } from 'react'
import { fetchMe, loginUrl, logout, type Me } from './auth.ts'

type View =
  | { kind: 'loading' }
  | { kind: 'signed-out' }
  | { kind: 'signed-in'; me: Me }
  | { kind: 'error' }

export default function App() {
  const [view, setView] = useState<View>({ kind: 'loading' })

  useEffect(() => {
    // StrictMode runs this twice in development; refresh() is single-flight, so it's safe.
    let cancelled = false
    fetchMe()
      .then((me) => {
        if (!cancelled) setView(me ? { kind: 'signed-in', me } : { kind: 'signed-out' })
      })
      .catch(() => {
        if (!cancelled) setView({ kind: 'error' })
      })
    return () => {
      cancelled = true
    }
  }, [])

  async function signOut() {
    await logout()
    setView({ kind: 'signed-out' })
  }

  if (view.kind === 'signed-in') {
    const { me } = view
    return (
      <>
        <header className="topbar">
          <strong>Doodle Arena</strong>
          <span className="user">
            {me.avatar_url && (
              <img src={me.avatar_url} alt="" width={28} height={28} referrerPolicy="no-referrer" />
            )}
            Logged in as {me.display_name}
            <button type="button" onClick={() => void signOut()}>
              Sign out
            </button>
          </span>
        </header>
        <main>
          <h1>Doodle Arena</h1>
          <p>Rooms arrive in Stage 2.</p>
        </main>
      </>
    )
  }

  const cancelled = new URLSearchParams(window.location.search).get('login_error') === 'cancelled'
  return (
    <main>
      <h1>Doodle Arena</h1>
      <p>Draw, guess, and trade your drawings.</p>
      {view.kind === 'loading' && <p>Loading…</p>}
      {view.kind === 'error' && <p role="alert">Could not reach the server. Try again shortly.</p>}
      {view.kind === 'signed-out' && (
        <>
          {cancelled && <p role="status">Sign-in was cancelled.</p>}
          <a className="button" href={loginUrl('/')}>
            Sign in with Google
          </a>
        </>
      )}
    </main>
  )
}
