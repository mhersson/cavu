import { useEffect, useRef, useState } from 'react'
import type { TaskEvent } from './types.ts'

type Options = { onEvent: (e: TaskEvent) => void; onReconnect: () => void; taskId?: number }

const firstDelay = 1000
const maxDelay = 30000

export function useEvents(o: Options): { live: boolean } {
  const [live, setLive] = useState(false)
  // Callbacks change between renders without reopening the stream.
  const callbacks = useRef(o)
  useEffect(() => {
    callbacks.current = o
  })
  const { taskId } = o

  useEffect(() => {
    let source: EventSource | null = null
    let timer: ReturnType<typeof setTimeout> | undefined
    let delay = firstDelay
    let lastId = 0
    let opened = false

    const open = () => {
      const params = new URLSearchParams()
      if (taskId !== undefined) params.set('task', String(taskId))
      // A source the browser could not keep reconnects by Last-Event-ID; a new one needs the id in the address.
      if (lastId > 0) params.set('after', String(lastId))
      const query = params.toString()
      const s = new EventSource('/api/events' + (query ? '?' + query : ''))
      source = s
      s.onopen = () => {
        setLive(true)
        delay = firstDelay
        if (opened) callbacks.current.onReconnect()
        opened = true
      }
      s.onmessage = (m: MessageEvent<string>) => {
        const e = JSON.parse(m.data) as TaskEvent
        lastId = e.id
        callbacks.current.onEvent(e)
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

    return () => {
      clearTimeout(timer)
      source?.close()
    }
  }, [taskId])

  return { live }
}
