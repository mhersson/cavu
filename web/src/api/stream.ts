import { useEffect, useRef, useState } from 'react'
import type { TaskEvent } from './types.ts'

type Subscriber = { event: (e: TaskEvent) => void; reconnect: () => void }

// The tab's one stream is the frame's; pages hear it through useStream.
const subscribers = new Set<Subscriber>()

const readGap = 2000

const kept = 'push_device'

// Browsers that block site data throw on access; then no id is kept.
export function device(): string | null {
  try {
    return localStorage.getItem(kept)
  } catch {
    return null
  }
}

// The stream carries the id, so a change of it opens the stream again.
let restart = () => {}

export function keepDevice(id: string | null) {
  if (id === device()) return
  if (id === null) localStorage.removeItem(kept)
  else localStorage.setItem(kept, id)
  restart()
}

const firstDelay = 1000
const maxDelay = 30000

export function useEvents(): { live: boolean } {
  const [live, setLive] = useState(false)

  useEffect(() => {
    let source: EventSource | null = null
    let timer: ReturnType<typeof setTimeout> | undefined
    let delay = firstDelay
    let lastId = 0
    let opened = false

    const open = () => {
      // The browser reconnects a source it keeps by sending Last-Event-ID; a new source after a closed one needs the id in the address.
      const q = new URLSearchParams()
      if (lastId > 0) q.set('after', String(lastId))
      const d = device()
      if (d !== null) q.set('device', d)
      const qs = q.toString()
      const s = new EventSource('/api/events' + (qs ? '?' + qs : ''))
      source = s
      s.onopen = () => {
        setLive(true)
        delay = firstDelay
        if (opened) subscribers.forEach((p) => p.reconnect())
        opened = true
      }
      s.onmessage = (m: MessageEvent<string>) => {
        const e = JSON.parse(m.data) as TaskEvent
        lastId = e.id
        subscribers.forEach((p) => p.event(e))
      }
      s.onerror = () => {
        setLive(false)
        if (s.readyState !== EventSource.CLOSED) return
        s.close()
        timer = setTimeout(open, delay)
        delay = Math.min(delay * 2, maxDelay)
      }
    }
    open()
    restart = () => {
      clearTimeout(timer)
      source?.close()
      delay = firstDelay
      open()
    }

    return () => {
      restart = () => {}
      clearTimeout(timer)
      source?.close()
    }
  }, [])

  return { live }
}

// f runs each time the stream opens again after its first opening; the result removes it.
export function onReopen(f: () => void): () => void {
  const sub: Subscriber = { event: () => {}, reconnect: f }
  subscribers.add(sub)
  return () => {
    subscribers.delete(sub)
  }
}

// read gets null for everything, or the ids of the tasks a burst of events was about.
export function useStream(read: (tasks: Set<number> | null) => void): void {
  // A new read between renders is used without resubscribing.
  const current = useRef(read)
  useEffect(() => {
    current.current = read
  })

  useEffect(() => {
    let lastRead = 0
    let timer: ReturnType<typeof setTimeout> | undefined
    let pending = new Set<number>()
    const run = (tasks: Set<number> | null) => {
      lastRead = Date.now()
      current.current(tasks)
    }
    const sub: Subscriber = {
      event: (e) => {
        pending.add(e.task_id)
        if (timer !== undefined) return
        timer = setTimeout(() => {
          timer = undefined
          const tasks = pending
          pending = new Set()
          run(tasks)
        }, Math.max(0, lastRead + readGap - Date.now()))
      },
      reconnect: () => {
        clearTimeout(timer)
        timer = undefined
        pending = new Set()
        run(null)
      },
    }
    subscribers.add(sub)
    run(null)
    return () => {
      subscribers.delete(sub)
      clearTimeout(timer)
    }
  }, [])
}
