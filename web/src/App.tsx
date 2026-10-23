import { useEffect, useState } from 'react'
import { fetchMe, loginUrl, logout, type Me } from './auth.ts'
import { Developer } from './developer/Developer.tsx'
import { DrawingPage } from './gallery/DrawingPage.tsx'
import { Gallery } from './gallery/Gallery.tsx'
import { Leaderboard } from './Leaderboard.tsx'
import { Lobby } from './Lobby.tsx'
import { Drops } from './market/Drops.tsx'
import { Market } from './market/Market.tsx'
import { MyDrawings } from './market/MyDrawings.tsx'
import { Wallet } from './market/Wallet.tsx'
import { Moderate } from './Moderate.tsx'
import { Room } from './room/Room.tsx'

type View =
  | { kind: 'loading' }
  | { kind: 'signed-out' }
  | { kind: 'signed-in'; me: Me }
  | { kind: 'error' }

/** The room id in a /rooms/<id> path (or /rooms/<id>/watch for a viewer), or null. */
function roomFromPath(path: string): { id: string; watch: boolean } | null {
  const m = /^\/rooms\/([a-z0-9]{1,64})(\/watch)?$/.exec(path)
  return m ? { id: m[1], watch: m[2] !== undefined } : null
}

/** The top bar's links, in order. */
const NAV: [path: string, label: string][] = [
  ['/', 'Lobby'],
  ['/gallery', 'Gallery'],
  ['/market', 'Market'],
  ['/drops', 'Drops'],
  ['/my-drawings', 'My drawings'],
  ['/wallet', 'Wallet'],
  ['/developer', 'Developer'],
]

/** The drawing id in a /drawings/<uuid> path. */
function drawingFromPath(path: string): string | null {
  return /^\/drawings\/([0-9a-f-]{36})$/.exec(path)?.[1] ?? null
}

export default function App() {
  const [view, setView] = useState<View>({ kind: 'loading' })
  const [path, setPath] = useState(() => window.location.pathname)
  const [search, setSearch] = useState(() => window.location.search)

  useEffect(() => {
    const onPop = () => {
      setPath(window.location.pathname)
      setSearch(window.location.search)
    }
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  function go(to: string) {
    window.history.pushState(null, '', to)
    const url = new URL(to, window.location.origin)
    setPath(url.pathname)
    setSearch(url.search)
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
    const room = roomFromPath(path)
    const drawingId = drawingFromPath(path)
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
          <nav className="nav">
            {NAV.map(([to, label]) => (
              <a
                key={to}
                href={to}
                aria-current={path === to ? 'page' : undefined}
                onClick={(e) => {
                  e.preventDefault()
                  go(to)
                }}
              >
                {label}
              </a>
            ))}
          </nav>
          <span className="user">
            {me.avatar_url && (
              <img src={me.avatar_url} alt="" width={28} height={28} referrerPolicy="no-referrer" />
            )}
            Logged in as {me.display_name}
            {(me.roles.includes('moderator') || me.roles.includes('admin')) && <a href="/admin/">Moderate</a>}
            <button type="button" onClick={() => void signOut()}>
              Sign out
            </button>
          </span>
        </header>
        {room ? (
          <Room
            key={path}
            roomId={room.id}
            watch={room.watch}
            onLeave={() => go('/')}
            onMatchDrawings={(matchId) => go(`/gallery?match_id=${encodeURIComponent(matchId)}`)}
          />
        ) : drawingId ? (
          <DrawingPage
            key={drawingId}
            drawingId={drawingId}
            meId={me.id}
            onBack={() => go('/gallery')}
            onWallet={() => go('/wallet')}
          />
        ) : path === '/gallery' ? (
          <Gallery
            key={search}
            meId={me.id}
            matchId={new URLSearchParams(search).get('match_id')}
            onOpen={(id) => go(`/drawings/${id}`)}
            onBack={() => go('/')}
          />
        ) : path === '/market' ? (
          <Market meId={me.id} onOpen={(id) => go(`/drawings/${id}`)} onWallet={() => go('/wallet')} />
        ) : path === '/drops' ? (
          <Drops
            isAdmin={me.roles.includes('admin')}
            onOpen={(id) => go(`/drawings/${id}`)}
            onWallet={() => go('/wallet')}
          />
        ) : path === '/my-drawings' ? (
          <MyDrawings onOpen={(id) => go(`/drawings/${id}`)} />
        ) : path === '/wallet' ? (
          <Wallet
            key={search}
            checkout={new URLSearchParams(search).get('checkout')}
            onClearCheckout={() => go('/wallet')}
          />
        ) : path === '/developer' ? (
          <Developer />
        ) : path === '/moderate' ? (
          <Moderate onBack={() => go('/')} />
        ) : path === '/leaderboard' ? (
          <Leaderboard meId={me.id} onBack={() => go('/')} />
        ) : (
          <Lobby
            onEnter={(id) => go(`/rooms/${id}`)}
            onWatch={(id) => go(`/rooms/${id}/watch`)}
            onLeaderboard={() => go('/leaderboard')}
            onGallery={() => go('/gallery')}
          />
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
          <a className="button" href={loginUrl(window.location.pathname + window.location.search)}>
            Sign in with Google
          </a>
        </>
      )}
    </main>
  )
}
