// The drawings (word, thumbnail) behind marketplace rows, which carry only drawing ids: one batch
// lookup per page of rows.

import { useEffect, useState } from 'react'
import { getDrawingsByIds, type Drawing } from '../gallery.ts'

export function useDrawings(ids: string[]): Map<string, Drawing> {
  const [known, setKnown] = useState<Map<string, Drawing>>(() => new Map())
  const missing = ids.filter((id) => !known.has(id))
  const wanted = [...new Set(missing)].slice(0, 50).join(',')

  useEffect(() => {
    if (!wanted) return
    let cancelled = false
    getDrawingsByIds(wanted.split(','))
      .then((ds) => {
        if (cancelled) return
        setKnown((prev) => {
          const next = new Map(prev)
          for (const d of ds) next.set(d.id, d)
          return next
        })
      })
      .catch(() => {}) // rows still show, with a placeholder instead of the picture
    return () => {
      cancelled = true
    }
  }, [wanted])

  return known
}
