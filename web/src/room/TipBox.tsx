// Tipping the drawers of a live room's round (ADR 0021): the coins leave the viewer's wallet at
// once and reach the drawers when the round ends. One Idempotency-Key per click, reused on retry.

import { useEffect, useState } from 'react'
import { ApiError } from '../api.ts'
import { useIdempotencyKeys } from '../idempotency.ts'
import { coins, getWallet, tip } from '../money.ts'

const TIP_AMOUNTS = [5, 10, 50]

type Props = {
  roomId: string
  matchId: string | null
  roundNo: number
  /** Who draws this round, for the label. */
  drawers: string
  /** A round is being chosen or drawn: tips go to its drawers. */
  open: boolean
}

/** The last tip's outcome, for the round it was sent in. */
type Result = { round: string; tone: 'good' | 'bad'; text: string } | null

export function TipBox({ roomId, matchId, roundNo, drawers, open }: Props) {
  const [balance, setBalance] = useState<number | null>(null)
  const [sending, setSending] = useState(false)
  const [result, setResult] = useState<Result>(null)
  const keys = useIdempotencyKeys()

  useEffect(() => {
    getWallet()
      .then((w) => setBalance(w.balance))
      .catch(() => setBalance(null))
  }, [])

  // A new round starts with a clean slate: an outcome shows only in the round it was sent in.
  const round = `${matchId}:${roundNo}`
  const shown = result?.round === round ? result : null

  const ready = open && matchId !== null && roundNo > 0 && !sending

  function send(amount: number) {
    if (!ready || matchId === null) return
    setSending(true)
    keys
      .run(`tip:${matchId}:${roundNo}:${amount}`, (key) => tip({ roomId, matchId, roundNo }, amount, key))
      .then(() => {
        setBalance((b) => (b === null ? null : b - amount))
        setResult({ round, tone: 'good', text: `You tipped ${coins(amount)}. It reaches ${drawers} when the round ends.` })
      })
      .catch((e: unknown) => {
        if (!(e instanceof ApiError)) {
          setResult({ round, tone: 'bad', text: 'Could not reach the server. Try again.' })
        } else if (e.code === 'INSUFFICIENT_FUNDS') {
          setResult({ round, tone: 'bad', text: `You don't have ${coins(amount)}.` })
        } else if (e.code === 'ROUND_OVER') {
          setResult({ round, tone: 'bad', text: 'This round is over. Tip the next one.' })
        } else if (e.code === 'IDEMPOTENCY_KEY_IN_PROGRESS' || e.status >= 500) {
          setResult({ round, tone: 'bad', text: 'The tip is still being processed. Try again.' })
        } else {
          setResult({ round, tone: 'bad', text: e.message })
        }
      })
      .finally(() => setSending(false))
  }

  return (
    <section className="tips" aria-label="Tips">
      <p className="muted">
        {open ? `Tip ${drawers}` : 'Tips open when the next round starts.'}
        {balance !== null && ` · You have ${coins(balance)}.`}
      </p>
      <div className="actions">
        {TIP_AMOUNTS.map((amount) => (
          <button key={amount} type="button" disabled={!ready} onClick={() => send(amount)}>
            Tip {amount}
          </button>
        ))}
      </div>
      {shown && (
        <p className={`notice ${shown.tone}`} role="status">
          {shown.text}
        </p>
      )}
    </section>
  )
}
