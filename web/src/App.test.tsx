import { useEffect } from 'react'
import { act, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, useLocation } from 'react-router'
import App from './App.tsx'
import { FakeEventSource, fakePush, fakeServer, json, narrow, redirected, rows } from './test-server.ts'

beforeEach(() => vi.useFakeTimers())
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

let here = ''
function Here() {
  const { pathname } = useLocation()
  useEffect(() => {
    here = pathname
  })
  return null
}

async function show(path = '/') {
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <App />
      <Here />
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
  await show('/board')
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
  await show('/board')
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

test('a narrow window shows the five tabs, a wide one the navigation', async () => {
  const links = () => within(screen.getByRole('navigation')).getAllByRole('link').map((a) => a.getAttribute('href'))
  fakeServer((c) => (needsYou(c) ? json({ tasks: [] }) : undefined))

  narrow(true)
  const view = await show()
  expect(links()).toEqual(['/', '/status', '/new', '/projects', '/more'])
  view.unmount()

  narrow(false)
  await show()
  expect(links()).toEqual(['/', '/board', '/projects', '/settings'])
})

test.each([
  ['/projects', '/projects'],
  ['/projects/cavu', '/projects'],
  ['/settings', '/more'],
  ['/status', '/status'],
  ['/new', '/new'],
])('the phone page %s marks %s as its current tab', async (path, tab) => {
  fakeServer((c) => (needsYou(c) ? json({ tasks: [] }) : undefined))
  narrow(true)
  await show(path)
  const nav = within(screen.getByRole('navigation', { name: 'Main' }))
  expect(nav.getByRole('link', { current: 'page' })).toHaveAttribute('href', tab)
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
  expect(screen.queryByRole('alert')).toBeNull()

  const user = userEvent.setup()
  await user.type(screen.getByRole('textbox'), 'ada')
  await user.type(password()!, 'pw')
  await user.click(screen.getByRole('button'))
  expect(await screen.findByRole('link', { name: /board/i })).toHaveAttribute('aria-current', 'page')
  expect(password()).toBeNull()
  expect(calls.at(-1)?.path).not.toBe('/api/auth/login')
})

test('a redirect to another origin asks for a reload', async () => {
  for (const phone of [false, true]) {
    let lapsed = true
    const calls = fakeServer((c) => (lapsed ? redirected() : needsYou(c) ? json({ tasks: [] }) : undefined))
    narrow(phone)
    const view = await show('/board')
    const notice = screen.getByRole('alert')
    expect(within(notice).getAllByRole('button')).toHaveLength(1)
    expect(within(notice).getByRole('button', { name: 'Reload' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument()

    lapsed = false
    const before = calls.filter(needsYou).length
    const es = FakeEventSource.all[0]
    act(() => es.open())
    act(() => es.fail())
    act(() => es.open())
    await act(() => vi.advanceTimersByTimeAsync(0))
    expect(calls.filter(needsYou).length).toBeGreaterThan(before)
    expect(screen.getByRole('alert')).toBe(notice)
    view.unmount()
  }
})

test('a network failure shows no notice', async () => {
  vi.stubGlobal('EventSource', FakeEventSource)
  const failing = vi.fn(() => Promise.reject(new TypeError('Failed to fetch')))
  vi.stubGlobal('fetch', failing)
  await show('/board')
  expect(failing).toHaveBeenCalled()
  expect(screen.queryByRole('alert')).toBeNull()
})

test('sign-out ends the session', async () => {
  vi.useRealTimers()
  const calls = fakeServer((c) => {
    if (needsYou(c)) return json({ tasks: [] })
    if (c.path === '/api/projects') return json({ projects: [{ runnable: true }] })
    if (c.path === '/api/auth/logout') return new Response(null, { status: 204 })
  })
  const user = userEvent.setup()
  const logouts = () => calls.filter((c) => c.path === '/api/auth/logout')

  narrow(false)
  const wide = await show('/board')
  await user.click(screen.getByRole('button', { name: 'Sign out' }))
  expect(await screen.findByLabelText(/password/i)).toBeInTheDocument()
  expect(logouts()).toHaveLength(1)
  expect(logouts()[0].method).toBe('POST')
  wide.unmount()

  narrow(true)
  await show('/more')
  await user.click(screen.getByRole('button', { name: 'Sign out' }))
  expect(await screen.findByLabelText(/password/i)).toBeInTheDocument()
  expect(logouts()).toHaveLength(2)
})

test('sign-out turns notifications off before it ends the session', async () => {
  vi.useRealTimers()
  fakePush({ subscribed: true })
  localStorage.setItem('push_device', '7')
  const calls = fakeServer((c) => {
    if (needsYou(c)) return json({ tasks: [] })
    if (c.path === '/api/projects') return json({ projects: [{ runnable: true }] })
    if (c.path === '/api/push') return json({ id: 7 })
    if (c.path === '/api/push/7' || c.path === '/api/auth/logout') return new Response(null, { status: 204 })
  })
  const user = userEvent.setup()
  await show('/board')
  await user.click(screen.getByRole('button', { name: 'Sign out' }))
  expect(await screen.findByLabelText(/password/i)).toBeInTheDocument()
  const sent = calls.filter((c) => c.path === '/api/push/7' || c.path === '/api/auth/logout').map((c) => [c.method, c.path])
  expect(sent).toEqual([['DELETE', '/api/push/7'], ['POST', '/api/auth/logout']])
})

test('sign-out does not wait for a worker that is never ready', async () => {
  vi.useRealTimers()
  fakePush({ ready: false })
  const calls = fakeServer((c) => {
    if (needsYou(c)) return json({ tasks: [] })
    if (c.path === '/api/projects') return json({ projects: [{ runnable: true }] })
    if (c.path === '/api/auth/logout') return new Response(null, { status: 204 })
  })
  const user = userEvent.setup()
  await show('/board')
  await user.click(screen.getByRole('button', { name: 'Sign out' }))
  expect(await screen.findByLabelText(/password/i)).toBeInTheDocument()
  expect(calls.filter((c) => c.path === '/api/auth/logout')).toHaveLength(1)
})

const projects = (c: { path: string }) => c.path === '/api/projects'

test('an instance with no project is sent to setup', async () => {
  fakeServer((c) => {
    if (projects(c)) return json({ projects: [] })
    if (needsYou(c)) return json({ tasks: [] })
  })
  await show('/board')
  expect(here).toBe('/setup')
  expect(screen.queryByRole('link', { name: /needs you/i })).toBeNull()
})

test('the frame checks for setup only when mounted', async () => {
  vi.useRealTimers()
  const user = userEvent.setup()
  let configured = true
  fakeServer((c) => {
    if (projects(c)) return json({ projects: configured ? [{ id: 1, name: 'web', runnable: true, baseline: { passed: true } }] : [] })
    if (needsYou(c)) return json({ tasks: [] })
  })
  narrow(true)
  await show('/')
  expect(here).toBe('/')
  configured = false
  await user.click(screen.getByRole('link', { name: /status/i }))
  expect(here).toBe('/status')
  await user.click(screen.getByRole('link', { name: /needs you/i }))
  expect(here).toBe('/')
})

test('a failed read of the projects sends nowhere', async () => {
  fakeServer((c) => {
    if (projects(c)) return json({ error: 'boom' }, 500)
    if (needsYou(c)) return json({ tasks: [] })
  })
  await show('/board')
  expect(screen.getByRole('link', { name: /board/i })).toHaveAttribute('aria-current', 'page')
})
