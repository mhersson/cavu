import { act, renderHook } from '@testing-library/react'
import { useEvents } from './stream.ts'
import type { TaskEvent } from './types.ts'

class FakeEventSource {
  static CONNECTING = 0
  static OPEN = 1
  static CLOSED = 2
  static all: FakeEventSource[] = []
  readyState = FakeEventSource.CONNECTING
  onopen: (() => void) | null = null
  onmessage: ((m: MessageEvent<string>) => void) | null = null
  onerror: (() => void) | null = null
  closed = false

  url: string

  constructor(url: string) {
    this.url = url
    FakeEventSource.all.push(this)
  }

  close() {
    this.closed = true
    this.readyState = FakeEventSource.CLOSED
  }

  open() {
    this.readyState = FakeEventSource.OPEN
    this.onopen?.()
  }

  send(id: number) {
    this.onmessage?.({ data: JSON.stringify({ id }) } as MessageEvent<string>)
  }

  // A closed source is one the browser gave up on, as after a 502 or a 401.
  fail(closed: boolean) {
    this.readyState = closed ? FakeEventSource.CLOSED : FakeEventSource.CONNECTING
    this.onerror?.()
  }
}

beforeEach(() => {
  vi.useFakeTimers()
  FakeEventSource.all = []
  vi.stubGlobal('EventSource', FakeEventSource)
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

function setup(taskId?: number) {
  const onEvent = vi.fn<(e: TaskEvent) => void>()
  const onReconnect = vi.fn()
  const hook = renderHook(() => useEvents({ onEvent, onReconnect, taskId }))
  return { onEvent, onReconnect, ...hook }
}

const last = () => FakeEventSource.all[FakeEventSource.all.length - 1]

test('stream reconnects from the last event id', () => {
  const { onEvent } = setup(7)
  expect(last().url).toBe('/api/events?task=7')
  act(() => last().open())
  act(() => last().send(41))
  act(() => last().send(42))
  expect(onEvent).toHaveBeenCalledTimes(2)

  act(() => last().fail(true))
  expect(FakeEventSource.all).toHaveLength(1)
  act(() => vi.advanceTimersByTime(1000))
  expect(FakeEventSource.all).toHaveLength(2)
  expect(last().url).toBe('/api/events?task=7&after=42')

  // The delay doubles while the stream does not open, to a cap.
  act(() => last().fail(true))
  act(() => vi.advanceTimersByTime(1999))
  expect(FakeEventSource.all).toHaveLength(2)
  act(() => vi.advanceTimersByTime(1))
  expect(FakeEventSource.all).toHaveLength(3)
  for (let i = 0; i < 8; i++) {
    act(() => last().fail(true))
    act(() => vi.advanceTimersByTime(30000))
  }
  const n = FakeEventSource.all.length
  act(() => last().fail(true))
  act(() => vi.advanceTimersByTime(30000))
  expect(FakeEventSource.all).toHaveLength(n + 1)

  // An open resets the delay.
  act(() => last().open())
  act(() => last().fail(true))
  act(() => vi.advanceTimersByTime(1000))
  expect(FakeEventSource.all).toHaveLength(n + 2)
})

test("a reconnect reads the screen's data again", () => {
  const { onReconnect } = setup()
  expect(last().url).toBe('/api/events')
  act(() => last().open())
  expect(onReconnect).not.toHaveBeenCalled()

  // The browser reconnects a source that is still connecting by itself.
  act(() => last().fail(false))
  act(() => last().open())
  expect(onReconnect).toHaveBeenCalledTimes(1)
  expect(FakeEventSource.all).toHaveLength(1)

  act(() => last().fail(true))
  act(() => vi.advanceTimersByTime(1000))
  act(() => last().open())
  expect(onReconnect).toHaveBeenCalledTimes(2)
})

test('live is false after an error and true at the next open', () => {
  const { result, unmount } = setup()
  expect(result.current.live).toBe(false)
  act(() => last().open())
  expect(result.current.live).toBe(true)
  act(() => last().fail(false))
  expect(result.current.live).toBe(false)
  act(() => last().open())
  expect(result.current.live).toBe(true)

  const source = last()
  unmount()
  expect(source.closed).toBe(true)
})

test('new callbacks do not reopen the stream', () => {
  const first = vi.fn()
  const second = vi.fn()
  const { rerender } = renderHook(
    ({ f }) => useEvents({ onEvent: f, onReconnect: () => {} }),
    { initialProps: { f: first } },
  )
  rerender({ f: second })
  expect(FakeEventSource.all).toHaveLength(1)
  act(() => last().send(1))
  expect(first).not.toHaveBeenCalled()
  expect(second).toHaveBeenCalledTimes(1)
})
