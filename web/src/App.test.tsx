import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import App from './App.tsx'

describe('App', () => {
  it('shows the game title', () => {
    render(<App />)
    expect(screen.getByRole('heading', { name: 'Doodle Arena' })).toBeTruthy()
  })
})
