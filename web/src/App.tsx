import { useEffect, useState } from 'react'
import { fetchMe, loginUrl, logout, type Me } from './auth.ts'
import { Leaderboard } from './Leaderboard.tsx'
import { Lobby } from './Lobby.tsx'
import { Room } from './room/Room.tsx'

type View =
  | { kind: 'loading' }
  | { kind: 'signed-out' }
  | { kind: 'signed-in'; me: Me }
  | { kind: 'error' }

/** The room id in a /rooms/<id> path, or null for the lobby. */
function roomFromPath(path: string): string | null {
  return /^\/rooms\/([a-z0-9]{1,64})$/.exec(path)?.[1] ?? null
}

export default function App() {
  const [view, setView] = useState<View>({ kind: 'loading' })
  const [path, setPath] = useState(() => window.location.pathname)

  useEffect(() => {
    const onPop = () => setPath(window.location.pathname)
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  function go(to: string) {
    window.history.pushState(null, '', to)
    setPath(to)
  }

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
    const roomId = roomFromPath(path)
    return (
      <>
        <header className="topbar">
          <a
            href="/"
            className="brand"
            onClick={(e) => {
              e.preventDefault()
              go('/')
            }}
          >
            Doodle Arena
          </a>
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
        {roomId ? (
          <Room key={roomId} roomId={roomId} onLeave={() => go('/')} />
        ) : path === '/leaderboard' ? (
          <Leaderboard meId={me.id} onBack={() => go('/')} />
        ) : (
          <Lobby onEnter={(id) => go(`/rooms/${id}`)} onLeaderboard={() => go('/leaderboard')} />
        )}
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
          <a className="button" href={loginUrl(window.location.pathname)}>
            Sign in with Google
          </a>
        </>
      )}
    </main>
  )
}
