// A drawing's thumbnail and word, linking to its page; a placeholder until the lookup answers.

import type { Drawing } from '../gallery.ts'

type Props = { drawingId: string; drawing: Drawing | undefined; onOpen: (drawingId: string) => void }

export function DrawingThumb({ drawingId, drawing, onOpen }: Props) {
  return (
    <a
      href={`/drawings/${drawingId}`}
      className="drawing-card"
      onClick={(e) => {
        e.preventDefault()
        onOpen(drawingId)
      }}
    >
      {drawing?.thumb_url ? (
        <img src={drawing.thumb_url} alt={`A drawing of "${drawing.word}"`} width={256} height={192} loading="lazy" />
      ) : (
        <span className="thumb-placeholder">{drawing ? 'No image yet' : '…'}</span>
      )}
      <span className="drawing-word">{drawing?.word ?? 'Drawing'}</span>
    </a>
  )
}
