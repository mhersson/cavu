import { render, screen } from '@testing-library/react'
import App from './App.tsx'

test('shows the app name as a heading', () => {
  render(<App />)
  expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument()
})
