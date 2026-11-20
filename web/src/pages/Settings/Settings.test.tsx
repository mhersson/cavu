import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import Settings from './Settings.tsx'

test('choosing a palette and a theme sets the root and the stored keys', async () => {
  render(<Settings />)
  const user = userEvent.setup()
  const [palette, theme] = screen.getAllByRole('radiogroup')
  const root = document.documentElement

  const radios = (g: HTMLElement) => within(g).getAllByRole('radio')
  expect(radios(palette).map((r) => r.getAttribute('aria-checked'))).toEqual(['true', 'false'])

  await user.click(radios(palette)[1])
  expect(root.dataset.palette).toBe('catppuccin')
  expect(localStorage.getItem('palette')).toBe('catppuccin')
  expect(radios(palette).map((r) => r.getAttribute('aria-checked'))).toEqual(['false', 'true'])

  await user.click(radios(theme)[2])
  expect(root.dataset.theme).toBe('light')
  expect(localStorage.getItem('theme')).toBe('light')
  expect(radios(theme).map((r) => r.getAttribute('aria-checked'))).toEqual(['false', 'false', 'true'])

  await user.click(radios(theme)[1])
  expect(root.dataset.theme).toBe('dark')
  expect(localStorage.getItem('theme')).toBe('dark')

  await user.click(radios(theme)[0])
  expect(localStorage.getItem('theme')).toBeNull()
  expect(localStorage.getItem('palette')).toBe('catppuccin')
})
