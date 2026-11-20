// Fakes of what a page talks to: fetch, EventSource and the browser's push.
export class FakeEventSource {
  static OPEN = 1
  static CLOSED = 2
  static all: FakeEventSource[] = []
  readyState = 0
  onopen: (() => void) | null = null
  onmessage: ((m: MessageEvent<string>) => void) | null = null
  onerror: (() => void) | null = null
  url: string

  constructor(url: string) {
    this.url = url
    FakeEventSource.all.push(this)
  }

  close() {
    this.readyState = FakeEventSource.CLOSED
  }

  open() {
    this.readyState = FakeEventSource.OPEN
    this.onopen?.()
  }

  send(id: number, more: object = {}) {
    this.onmessage?.({ data: JSON.stringify({ id, ...more }) } as MessageEvent<string>)
  }

  fail() {
    this.readyState = 0
    this.onerror?.()
  }
}

export const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

// What fetch resolves with for a redirect it was told not to follow; no Response can be made with this type and status.
export const redirected = () =>
  Object.defineProperties(new Response(null), { type: { value: 'opaqueredirect' }, status: { value: 0 } })

export const rows = (n: number) =>
  Array.from({ length: n }, (_, i) => ({ id: i + 1, needs_you: true }))

export type Call = { method: string; path: string; body: unknown; headers: Headers; keepalive?: boolean }

// answer maps a request to its response; every request is recorded.
export function fakeServer(answer: (c: Call) => Response | undefined) {
  const calls: Call[] = []
  vi.stubGlobal('EventSource', FakeEventSource)
  FakeEventSource.all = []
  vi.stubGlobal(
    'fetch',
    vi.fn((path: string, init: RequestInit = {}) => {
      const call: Call = {
        method: init.method ?? 'GET',
        path,
        body: init.body === undefined ? undefined : JSON.parse(init.body as string),
        headers: new Headers(init.headers),
        keepalive: init.keepalive,
      }
      calls.push(call)
      return Promise.resolve(answer(call) ?? json({ error: 'not found' }, 404))
    }),
  )
  return calls
}

export function narrow(matches: boolean) {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }))
}

type PushOptions = {
  supported?: boolean
  secure?: boolean
  standalone?: boolean
  permission?: NotificationPermission
  answer?: NotificationPermission
  subscribed?: boolean
  ready?: boolean
}

// A browser with Web Push, or with none (supported: false), or whose worker is never ready; returns what a test inspects.
export function fakePush({ supported = true, secure = true, standalone, permission = 'default', answer = 'granted', subscribed = false, ready = true }: PushOptions = {}) {
  vi.stubGlobal('isSecureContext', secure)
  Reflect.deleteProperty(navigator, 'serviceWorker')
  Reflect.deleteProperty(navigator, 'standalone')
  if (standalone !== undefined) Object.defineProperty(navigator, 'standalone', { value: standalone, configurable: true })
  const unsubscribe = vi.fn(() => Promise.resolve(true))
  const subscription = {
    endpoint: 'https://push.example/s1',
    toJSON: () => ({ endpoint: 'https://push.example/s1', expirationTime: null, keys: { p256dh: 'P', auth: 'A', extra: 'X' } }),
    unsubscribe,
  }
  let current: typeof subscription | null = subscribed ? subscription : null
  const subscribe = vi.fn<(options: PushSubscriptionOptionsInit) => Promise<typeof subscription>>(() => {
    current = subscription
    return Promise.resolve(subscription)
  })
  const registration = { pushManager: { getSubscription: () => Promise.resolve(current), subscribe } }
  const requestPermission = vi.fn(() => Promise.resolve(answer))
  if (supported) {
    Object.defineProperty(navigator, 'serviceWorker', { value: { ready: ready ? Promise.resolve(registration) : new Promise(() => {}) }, configurable: true })
    vi.stubGlobal('PushManager', class {})
    vi.stubGlobal('Notification', { permission, requestPermission })
  }
  return { subscribe, unsubscribe, requestPermission }
}
