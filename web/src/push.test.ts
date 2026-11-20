import { renderHook, waitFor } from '@testing-library/react'
import worker from '../public/sw.js?raw'
import { fakePush, fakeServer, json, type Call } from './test-server.ts'
import { pushState, turnOff, turnOn, useFocusReport } from './push.ts'

afterEach(() => vi.unstubAllGlobals())

const puts = (calls: Call[]) => calls.filter((c) => c.method === 'PUT' && c.path === '/api/push').map((c) => c.body)

function server() {
  return fakeServer((c) => {
    if (c.path === '/api/push' && c.method === 'PUT') return json({ id: 7 })
    if (c.path.startsWith('/api/push/') && c.method === 'DELETE') return new Response(null, { status: 204 })
  })
}

test('pushState says what a browser can do and whether it is on', () => {
  fakePush({ supported: false, secure: false })
  expect(pushState()).toBe('unsupported')
  fakePush({ supported: false, standalone: true })
  expect(pushState()).toBe('unsupported')
  fakePush({ supported: false, standalone: false })
  expect(pushState()).toBe('install-first')
  fakePush({ supported: false, standalone: false, secure: false })
  expect(pushState()).toBe('unsupported')
  fakePush({ permission: 'denied' })
  expect(pushState()).toBe('denied')
  fakePush()
  expect(pushState()).toBe('off')
  localStorage.setItem('push_device', '7')
  expect(pushState()).toBe('on')
})

test('turnOn asks, subscribes with the key and reports the subscription', async () => {
  const push = fakePush()
  const calls = server()
  const done = turnOn('KEY')
  // The permission is asked before anything is awaited, so it stays inside the click.
  expect(push.requestPermission).toHaveBeenCalledTimes(1)
  await done
  expect(push.subscribe).toHaveBeenCalledWith({ userVisibleOnly: true, applicationServerKey: 'KEY' })
  expect(puts(calls)).toEqual([{ endpoint: 'https://push.example/s1', keys: { p256dh: 'P', auth: 'A' }, origin: location.origin, focused: true }])
  expect(localStorage.getItem('push_device')).toBe('7')
})

test('the body holds exactly the fields the server accepts', async () => {
  fakePush()
  const calls = server()
  await turnOn('KEY')
  const body = puts(calls)[0] as { keys: object }
  expect(Object.keys(body).sort()).toEqual(['endpoint', 'focused', 'keys', 'origin'])
  expect(Object.keys(body.keys).sort()).toEqual(['auth', 'p256dh'])
})

test('a refused permission subscribes to nothing and sends nothing', async () => {
  const push = fakePush({ answer: 'denied' })
  const calls = server()
  await turnOn('KEY')
  expect(push.subscribe).not.toHaveBeenCalled()
  expect(calls).toEqual([])
  expect(localStorage.getItem('push_device')).toBeNull()
})

test('turnOff unsubscribes, deletes the server side and forgets the id', async () => {
  const push = fakePush({ subscribed: true })
  localStorage.setItem('push_device', '7')
  const calls = server()
  await turnOff()
  expect(push.unsubscribe).toHaveBeenCalledTimes(1)
  expect(calls.map((c) => [c.method, c.path])).toEqual([['DELETE', '/api/push/7']])
  expect(localStorage.getItem('push_device')).toBeNull()
})

test('turnOff counts a 404 as removed and keeps the id after another failure', async () => {
  fakePush({ subscribed: true })
  localStorage.setItem('push_device', '7')
  fakeServer(() => json({ error: 'gone' }, 404))
  await turnOff()
  expect(localStorage.getItem('push_device')).toBeNull()

  localStorage.setItem('push_device', '7')
  fakeServer(() => json({ error: 'broken' }, 500))
  await expect(turnOff()).rejects.toThrow()
  expect(localStorage.getItem('push_device')).toBe('7')
})

test('the page reports focus, blur and visibility while an id is kept', async () => {
  fakePush({ subscribed: true })
  localStorage.setItem('push_device', '7')
  const calls = server()
  const focus = vi.spyOn(document, 'hasFocus').mockReturnValue(false)
  renderHook(() => useFocusReport())
  await waitFor(() => expect(puts(calls)).toHaveLength(1))
  expect(puts(calls)[0]).toMatchObject({ endpoint: 'https://push.example/s1', focused: false })

  window.dispatchEvent(new Event('focus'))
  await waitFor(() => expect(puts(calls)).toHaveLength(2))
  expect(puts(calls)[1]).toMatchObject({ focused: true })

  window.dispatchEvent(new Event('blur'))
  await waitFor(() => expect(puts(calls)).toHaveLength(3))
  expect(puts(calls)[2]).toMatchObject({ focused: false })

  focus.mockReturnValue(true)
  document.dispatchEvent(new Event('visibilitychange'))
  await waitFor(() => expect(puts(calls)).toHaveLength(4))
  expect(puts(calls)[3]).toMatchObject({ focused: true })
  focus.mockRestore()
})

test('the page reports nothing while no id is kept and forgets an id the browser has no subscription for', async () => {
  fakePush()
  const calls = server()
  const { unmount } = renderHook(() => useFocusReport())
  window.dispatchEvent(new Event('focus'))
  window.dispatchEvent(new Event('blur'))
  await new Promise((r) => setTimeout(r))
  expect(calls).toEqual([])
  unmount()

  localStorage.setItem('push_device', '7')
  renderHook(() => useFocusReport())
  await waitFor(() => expect(localStorage.getItem('push_device')).toBeNull())
  expect(calls).toEqual([])
})

test('a page with a subscription and no kept id keeps the id the server gives', async () => {
  fakePush({ subscribed: true })
  server()
  renderHook(() => useFocusReport())
  await waitFor(() => expect(localStorage.getItem('push_device')).toBe('7'))
})

test('a page that cannot push sends nothing', async () => {
  fakePush({ supported: false })
  const calls = server()
  renderHook(() => useFocusReport())
  window.dispatchEvent(new Event('focus'))
  await new Promise((r) => setTimeout(r))
  expect(calls).toEqual([])
})

test('a report that fails is dropped and keeps the id', async () => {
  fakePush({ subscribed: true })
  localStorage.setItem('push_device', '7')
  const calls = fakeServer(() => json({ error: 'broken' }, 500))
  renderHook(() => useFocusReport())
  await waitFor(() => expect(calls).toHaveLength(1))
  expect(localStorage.getItem('push_device')).toBe('7')
})

// The service worker, run against a recording of the browser's side.
function load() {
  const listeners: Record<string, (e: unknown) => void> = {}
  const self = {
    location: { origin: 'https://cavu.example' },
    registration: { showNotification: vi.fn(() => Promise.resolve()) },
    clients: { matchAll: vi.fn(), openWindow: vi.fn(() => Promise.resolve()), claim: vi.fn(() => Promise.resolve()) },
    addEventListener: (type: string, f: (e: unknown) => void) => void (listeners[type] = f),
  }
  new Function('self', worker)(self)
  return { self, listeners }
}

const message = { title: 'Ready for review · cavu', body: 'Print the view', tag: 'task-5', url: '/tasks/5' }

test('every push shows its notification, the same task again as well', () => {
  const { self, listeners } = load()
  const waited: Promise<unknown>[] = []
  for (let i = 0; i < 2; i++) listeners.push({ data: { json: () => message }, waitUntil: (p: Promise<unknown>) => waited.push(p) })
  expect(self.registration.showNotification).toHaveBeenCalledTimes(2)
  expect(self.registration.showNotification).toHaveBeenCalledWith(message.title, expect.objectContaining({ body: message.body, tag: message.tag, data: { url: message.url } }))
  expect(waited).toHaveLength(2)
})

test('a click closes the notification and takes an open window to the task', async () => {
  const { self, listeners } = load()
  const win = { focus: vi.fn(() => Promise.resolve()), navigate: vi.fn(() => Promise.resolve()) }
  self.clients.matchAll.mockResolvedValue([win])
  const close = vi.fn()
  let waited: Promise<unknown> = Promise.resolve()
  listeners.notificationclick({ notification: { close, data: { url: '/tasks/5' } }, waitUntil: (p: Promise<unknown>) => (waited = p) })
  await waited
  expect(close).toHaveBeenCalled()
  expect(win.focus).toHaveBeenCalled()
  expect(win.navigate).toHaveBeenCalledWith('https://cavu.example/tasks/5')
  expect(self.clients.openWindow).not.toHaveBeenCalled()
})

test('a click with no window open opens one at the task', async () => {
  const { self, listeners } = load()
  self.clients.matchAll.mockResolvedValue([])
  let waited: Promise<unknown> = Promise.resolve()
  listeners.notificationclick({ notification: { close: vi.fn(), data: { url: '/tasks/5' } }, waitUntil: (p: Promise<unknown>) => (waited = p) })
  await waited
  expect(self.clients.openWindow).toHaveBeenCalledWith('https://cavu.example/tasks/5')
})

test('the worker claims its page when it activates', async () => {
  const { self, listeners } = load()
  let waited: Promise<unknown> = Promise.resolve()
  listeners.activate({ waitUntil: (p: Promise<unknown>) => (waited = p) })
  await waited
  expect(self.clients.claim).toHaveBeenCalled()
})
