// Fakes of the two things a page talks to: fetch and EventSource.
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

export const rows = (n: number) =>
  Array.from({ length: n }, (_, i) => ({ id: i + 1, needs_you: true }))

export type Call = { method: string; path: string; body: unknown; headers: Headers }

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
