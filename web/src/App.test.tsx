import { act, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import App from './App.tsx'
import { FakeEventSource, fakeServer, json, narrow, rows } from './test-server.ts'

beforeEach(() => vi.useFakeTimers())
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

async function show(path = '/') {
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  )
  await act(() => vi.advanceTimersByTimeAsync(0))
  return view
}

const needsYou = (c: { path: string }) => c.path === '/api/tasks?needs_you=true'

test('the navigation shows the count of tasks that need you', async () => {
  let n = 3
  const calls = fakeServer((c) => {
    if (needsYou(c)) return json({ tasks: rows(n) })
    if (c.path === '/api/health') return json({ status: 'ok', version: '1.2.3' })
  })
  await show()
  const link = () => screen.getByRole('link', { name: /needs you/i })
  expect(link()).toHaveTextContent('3')
  expect(calls.filter(needsYou)).toHaveLength(1)

  n = 5
  const es = FakeEventSource.all[0]
  act(() => es.open())
  for (let i = 1; i <= 4; i++) act(() => es.send(i))
  await act(() => vi.advanceTimersByTimeAsync(1900))
  expect(calls.filter(needsYou)).toHaveLength(1)
  await act(() => vi.advanceTimersByTimeAsync(200))
  expect(calls.filter(needsYou)).toHaveLength(2)
  expect(link()).toHaveTextContent('5')

  n = 0
  act(() => es.send(5))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  expect(link().querySelector('.count')).toBeNull()
})

test('the navigation says when the stream is down', async () => {
  fakeServer((c) => (needsYou(c) ? json({ tasks: [] }) : undefined))
  await show()
  const mark = screen.getByRole('status')
  expect(mark).toHaveAttribute('data-live', 'false')
  act(() => FakeEventSource.all[0].open())
  expect(mark).toHaveAttribute('data-live', 'true')
  act(() => FakeEventSource.all[0].fail())
  expect(mark).toHaveAttribute('data-live', 'false')
})

test('a narrow window shows the four tabs, a wide one the navigation', async () => {
  const links = () => screen.getAllByRole('link').map((a) => a.getAttribute('href'))
  fakeServer((c) => (needsYou(c) ? json({ tasks: [] }) : undefined))

  narrow(true)
  const view = await show()
  expect(links()).toEqual(['/', '/new', '/status', '/more'])
  view.unmount()

  narrow(false)
  await show()
  expect(links()).toEqual(['/', '/board', '/projects', '/settings'])
})

test('a path that is no screen goes to the first one', async () => {
  fakeServer((c) => (needsYou(c) ? json({ tasks: [] }) : undefined))
  await show('/no/such/page')
  expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument()
  expect(screen.getByRole('link', { name: /needs you/i })).toHaveAttribute('aria-current', 'page')
})
