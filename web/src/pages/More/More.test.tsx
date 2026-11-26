import { act, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { fakeServer, json } from '../../test-server.ts'
import More from './More.tsx'

afterEach(() => vi.unstubAllGlobals())

test('More links to settings and leaves projects to the tab bar', async () => {
  fakeServer((c) => {
    if (c.path === '/api/health') return json({ status: 'ok', version: '1.2.3' })
  })
  render(
    <MemoryRouter>
      <More />
    </MemoryRouter>,
  )
  await act(async () => {})
  expect(screen.getAllByRole('link').map((a) => a.getAttribute('href'))).toEqual(['/settings'])
})
