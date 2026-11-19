import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
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
  await act(async () => {
    if (vi.isFakeTimers()) await vi.advanceTimersByTimeAsync(0)
  })
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

test('the navigation reads the count again when the stream reconnects', async () => {
  let n = 1
  const calls = fakeServer((c) => (needsYou(c) ? json({ tasks: rows(n) }) : undefined))
  await show()
  const es = FakeEventSource.all[0]
  act(() => es.open())
  expect(calls.filter(needsYou)).toHaveLength(1)

  n = 4
  act(() => es.fail())
  act(() => es.open())
  await act(() => vi.advanceTimersByTimeAsync(0))
  expect(calls.filter(needsYou)).toHaveLength(2)
  expect(screen.getByRole('link', { name: /needs you/i })).toHaveTextContent('4')
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

test('a page reached through More keeps the More tab current', async () => {
  fakeServer((c) => (needsYou(c) ? json({ tasks: [] }) : undefined))
  narrow(true)
  const current = () =>
    screen.getAllByRole('link').filter((a) => a.classList.contains('on')).map((a) => a.getAttribute('href'))
  const view = await show('/projects/cavu')
  expect(current()).toEqual(['/more'])
  view.unmount()
  await show('/status')
  expect(current()).toEqual(['/status'])
})

test('a path that is no screen goes to the first one', async () => {
  fakeServer((c) => (needsYou(c) ? json({ tasks: [] }) : undefined))
  await show('/no/such/page')
  expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument()
  expect(screen.getByRole('link', { name: /needs you/i })).toHaveAttribute('aria-current', 'page')
})

const password = () => document.querySelector<HTMLInputElement>('input[type=password]')

test('a 401 opens sign-in and returns to the page', async () => {
  vi.useRealTimers()
  let signedIn = false
  const calls = fakeServer((c) => {
    if (c.path === '/api/auth/login') {
      signedIn = true
      return new Response(null, { status: 204 })
    }
    if (needsYou(c)) return signedIn ? json({ tasks: [] }) : json({ error: 'sign in' }, 401)
  })
  await show('/board?x=1')
  expect(await screen.findByLabelText(/password/i)).toBeInTheDocument()
  expect(screen.queryByRole('link', { name: /board/i })).toBeNull()

  const user = userEvent.setup()
  await user.type(screen.getByRole('textbox'), 'ada')
  await user.type(password()!, 'pw')
  await user.click(screen.getByRole('button'))
  expect(await screen.findByRole('link', { name: /board/i })).toHaveAttribute('aria-current', 'page')
  expect(password()).toBeNull()
  expect(calls.at(-1)?.path).not.toBe('/api/auth/login')
})

test('sign-out ends the session', async () => {
  vi.useRealTimers()
  const calls = fakeServer((c) => {
    if (needsYou(c)) return json({ tasks: [] })
    if (c.path === '/api/projects') return json({ projects: [] })
    if (c.path === '/api/auth/logout') return new Response(null, { status: 204 })
  })
  const user = userEvent.setup()
  const logouts = () => calls.filter((c) => c.path === '/api/auth/logout')

  narrow(false)
  const wide = await show('/board')
  await user.click(screen.getByRole('button'))
  expect(await screen.findByLabelText(/password/i)).toBeInTheDocument()
  expect(logouts()).toHaveLength(1)
  expect(logouts()[0].method).toBe('POST')
  wide.unmount()

  narrow(true)
  await show('/more')
  await user.click(screen.getByRole('button'))
  expect(await screen.findByLabelText(/password/i)).toBeInTheDocument()
  expect(logouts()).toHaveLength(2)
})
