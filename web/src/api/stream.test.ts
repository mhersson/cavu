import { act, renderHook } from '@testing-library/react'
import { keepDevice, useEvents, useStream } from './stream.ts'

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

  send(id: number, more: object = {}) {
    this.onmessage?.({ data: JSON.stringify({ id, ...more }) } as MessageEvent<string>)
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
  vi.setSystemTime(10_000)
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const last = () => FakeEventSource.all[FakeEventSource.all.length - 1]

// The frame's stream, and a page that subscribes to it.
function setup() {
  const read = vi.fn<(tasks: Set<number> | null) => void>()
  const frame = renderHook(() => useEvents())
  const page = renderHook(() => useStream(read))
  return { read, frame, page }
}

test('stream reconnects from the last event id', () => {
  setup()
  expect(last().url).toBe('/api/events')
  act(() => last().open())
  act(() => last().send(41))
  act(() => last().send(42))

  act(() => last().fail(true))
  expect(FakeEventSource.all).toHaveLength(1)
  act(() => vi.advanceTimersByTime(1000))
  expect(FakeEventSource.all).toHaveLength(2)
  expect(last().url).toBe('/api/events?after=42')

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
  const { read } = setup()
  act(() => last().open())
  read.mockClear()

  // The browser reconnects a source that is still connecting by itself.
  act(() => last().fail(false))
  act(() => last().open())
  expect(read).toHaveBeenCalledTimes(1)
  expect(read).toHaveBeenLastCalledWith(null)
  expect(FakeEventSource.all).toHaveLength(1)

  act(() => last().fail(true))
  act(() => vi.advanceTimersByTime(1000))
  act(() => last().open())
  expect(read).toHaveBeenCalledTimes(2)
})

test('live is false after an error and true at the next open', () => {
  const { frame } = setup()
  expect(frame.result.current.live).toBe(false)
  act(() => last().open())
  expect(frame.result.current.live).toBe(true)
  act(() => last().fail(false))
  expect(frame.result.current.live).toBe(false)
  act(() => last().open())
  expect(frame.result.current.live).toBe(true)

  const source = last()
  frame.unmount()
  expect(source.closed).toBe(true)
})

test('a new read does not reopen the stream', () => {
  const first = vi.fn()
  const second = vi.fn()
  renderHook(() => useEvents())
  const { rerender } = renderHook(({ f }) => useStream(f), { initialProps: { f: first } })
  rerender({ f: second })
  expect(FakeEventSource.all).toHaveLength(1)
  first.mockClear()
  act(() => last().send(1, { task_id: 3 }))
  act(() => vi.advanceTimersByTime(2000))
  expect(first).not.toHaveBeenCalled()
  expect(second).toHaveBeenCalledTimes(1)
})

test('a page reads once when it mounts', () => {
  const { read } = setup()
  expect(read).toHaveBeenCalledTimes(1)
  expect(read).toHaveBeenCalledWith(null)
})

test("a page hears the frame's events: a burst leads to one read with the ids of its tasks", () => {
  const { read } = setup()
  act(() => last().open())
  read.mockClear()

  // The mount read was just now, so the burst waits out the gap.
  act(() => last().send(1, { task_id: 7 }))
  act(() => last().send(2, { task_id: 7 }))
  act(() => last().send(3, { task_id: 9 }))
  expect(read).not.toHaveBeenCalled()
  act(() => vi.advanceTimersByTime(2000))
  expect(read).toHaveBeenCalledTimes(1)
  expect(read).toHaveBeenCalledWith(new Set([7, 9]))

  // The last read is old enough, so the next burst reads at once.
  act(() => vi.advanceTimersByTime(5000))
  act(() => last().send(4, { task_id: 8 }))
  act(() => vi.advanceTimersByTime(0))
  expect(read).toHaveBeenCalledTimes(2)
  expect(read).toHaveBeenLastCalledWith(new Set([8]))
})

test('a reconnect reads everything at once', () => {
  const { read } = setup()
  act(() => last().open())
  read.mockClear()
  act(() => last().send(1, { task_id: 7 }))
  act(() => last().fail(false))
  act(() => last().open())
  expect(read).toHaveBeenCalledTimes(1)
  expect(read).toHaveBeenCalledWith(null)

  // The pending burst was folded into the reconnect's read.
  act(() => vi.advanceTimersByTime(2000))
  expect(read).toHaveBeenCalledTimes(1)
})

test('a page that unmounts hears nothing more', () => {
  const { read, page } = setup()
  act(() => last().open())
  act(() => last().send(1, { task_id: 7 }))
  read.mockClear()
  page.unmount()
  act(() => vi.advanceTimersByTime(5000))
  act(() => last().send(2, { task_id: 7 }))
  act(() => last().fail(false))
  act(() => last().open())
  act(() => vi.advanceTimersByTime(5000))
  expect(read).not.toHaveBeenCalled()
})

test('the stream carries the kept device and opens again when it changes', () => {
  localStorage.setItem('push_device', '7')
  setup()
  expect(last().url).toBe('/api/events?device=7')
  act(() => last().open())
  act(() => last().send(42))

  act(() => keepDevice('8'))
  expect(FakeEventSource.all).toHaveLength(2)
  expect(FakeEventSource.all[0].closed).toBe(true)
  expect(last().url).toBe('/api/events?after=42&device=8')

  act(() => keepDevice(null))
  expect(FakeEventSource.all).toHaveLength(3)
  expect(last().url).toBe('/api/events?after=42')
  expect(localStorage.getItem('push_device')).toBeNull()

  act(() => keepDevice(null))
  expect(FakeEventSource.all).toHaveLength(3)
})
