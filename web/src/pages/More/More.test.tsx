import { act, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { fakeServer, json } from '../../test-server.ts'
import More from './More.tsx'

afterEach(() => vi.unstubAllGlobals())

test.each([
  [1, '1 project'],
  [3, '3 projects'],
])('More shows %i projects as %s', async (n, shown) => {
  fakeServer((c) => {
    if (c.path === '/api/projects') return json({ projects: Array.from({ length: n }, (_, i) => ({ id: i + 1 })) })
    if (c.path === '/api/health') return json({ status: 'ok', version: '1.2.3' })
  })
  render(
    <MemoryRouter>
      <More />
    </MemoryRouter>,
  )
  await act(async () => {})
  expect(await screen.findByText(shown)).toBeInTheDocument()
})
