import { useEffect, useEffectEvent, useRef, useState, type DragEvent } from 'react'
import { api } from './client.ts'
import type { Image, Settings, Task } from './types.ts'

// The types the server keeps, which it reads from a file's first bytes.
export const imageTypes = ['image/png', 'image/jpeg', 'image/gif', 'image/webp']

// A size as a person reads it.
export function size(n: number) {
  if (n < 1024) return `${n} B`
  if (n < 1 << 20) return `${Math.round(n / 1024)} KiB`
  return `${+(n / (1 << 20)).toFixed(1)} MiB`
}

// What a file is, by the end of its name.
export const ext = (name: string) => /\.(\w{1,8})$/.exec(name)?.[1].toLowerCase() ?? 'file'

// The line refusing a file the server would refuse, or '' for one that may go. A cap of 0 is not yet known, and a file
// the browser gives no type is the server's to judge.
export function refusal(f: File, cap: number) {
  if (f.type && !imageTypes.includes(f.type)) return `${f.name} is not an image CAVU shows. Use PNG, JPEG, GIF or WebP.`
  if (cap && f.size > cap) return `${f.name} is ${size(f.size)}. An image can be at most ${size(cap)}.`
  return ''
}

// The files that may go; refuse is given the line for the last that may not, or '' when all may.
export function fitting(files: File[], cap: number, refuse: (line: string) => void) {
  const no = files.map((f) => refusal(f, cap))
  refuse(no.findLast(Boolean) ?? '')
  return files.filter((_, i) => !no[i])
}

export const imageURL = (task: number, image: number) => `/api/tasks/${task}/images/${image}`

export const uploadImage = (task: number, f: File, signal?: AbortSignal) =>
  api<Image>('POST', `/tasks/${task}/images?${new URLSearchParams({ name: f.name })}`, f, { signal })

export const removeImage = (task: number, image: number) => api<Task>('PATCH', `/tasks/${task}`, { remove_images: [image] })

export type Upload = { key: number; name: string; size: number }

// refused is the line of the last upload or removal refused; removing holds the ids of the images whose removal is
// under way, and remove hands the task the server answers with to then.
export type Uploads = {
  cap: number
  going: Upload[]
  removing: number[]
  refused: string
  send: (files: File[]) => void
  remove: (image: number, then: (t: Task) => void) => void
}

// The server's cap on an image, 0 until read, and what ends every request sent with its signal once the page is left.
export function useCap() {
  const [cap, setCap] = useState(0)
  const ends = useRef<AbortController>(null)
  // Made in the effect, so the second run of a development build has its own.
  useEffect(() => {
    const c = new AbortController()
    ends.current = c
    api<Settings>('GET', '/settings', undefined, { signal: c.signal }).then((s) => setCap(s.image_max_bytes), () => {})
    return () => c.abort()
  }, [])
  return { cap, ends }
}

// Attaches images to the task with the id, each under way until done after it ends, and removes them, each under way
// until the server answers. A file the server would refuse is refused before it is sent, and an upload still under way
// ends when the page is left. first is a line to show at once.
export function useUploads(id: number, done: () => Promise<unknown>, first = ''): Uploads {
  const { cap, ends } = useCap()
  const [going, setGoing] = useState<Upload[]>([])
  const [removals, setRemovals] = useState<{ image: number }[]>([])
  const [refused, setRefused] = useState(first)
  const count = useRef(0)

  const send = (files: File[]) => {
    const { signal } = ends.current!
    for (const f of fitting(files, cap, setRefused)) {
      const u = { key: ++count.current, name: f.name, size: f.size }
      setGoing((g) => [...g, u])
      uploadImage(id, f, signal)
        .then(done, (e: unknown) => !signal.aborted && setRefused(e instanceof Error ? e.message : String(e)))
        .finally(() => setGoing((g) => g.filter((x) => x !== u)))
    }
  }
  // Each removal is its own entry, so one answered never ends another of the same image.
  const remove = (image: number, then: (t: Task) => void) => {
    const r = { image }
    setRemovals((rs) => [...rs, r])
    removeImage(id, image)
      .then(
        (t) => {
          setRefused('')
          then(t)
        },
        (e: unknown) => setRefused(e instanceof Error ? e.message : String(e)),
      )
      .finally(() => setRemovals((rs) => rs.filter((x) => x !== r)))
  }
  return { cap, going, removing: removals.map((r) => r.image), refused, send, remove }
}

// On a desktop, while on: an image pasted anywhere but into a field's text is sent, and so are files dropped on what
// takes the returned handlers, which is over while a file is dragged across it.
export function useDrop(on: boolean, send: (files: File[]) => void) {
  const [over, setOver] = useState(false)
  const pasted = useEffectEvent((e: ClipboardEvent) => {
    // Cells copied from a spreadsheet come as text and as a picture of them, and in a field the text is what was meant.
    const at = e.target instanceof Element ? e.target : null
    if (at?.closest('input, textarea') && e.clipboardData?.types.includes('text/plain')) return
    const files = [...(e.clipboardData?.files ?? [])].filter((f) => f.type.startsWith('image/'))
    if (files.length === 0) return
    e.preventDefault()
    send(files)
  })
  useEffect(() => {
    if (!on) return
    // A file dropped beside the target would have the browser leave the app to show it.
    const keep = (e: globalThis.DragEvent) => {
      if (e.defaultPrevented || !e.dataTransfer?.types.includes('Files')) return
      e.preventDefault()
      e.dataTransfer.dropEffect = 'none'
    }
    document.addEventListener('paste', pasted)
    document.addEventListener('dragover', keep)
    document.addEventListener('drop', keep)
    return () => {
      document.removeEventListener('paste', pasted)
      document.removeEventListener('dragover', keep)
      document.removeEventListener('drop', keep)
    }
  }, [on])

  const files = (e: DragEvent) => e.dataTransfer.types.includes('Files')
  const handlers = on
    ? {
        onDragOver: (e: DragEvent) => {
          if (!files(e)) return
          e.preventDefault()
          setOver(true)
        },
        onDragLeave: (e: DragEvent) => !e.currentTarget.contains(e.relatedTarget as Node) && setOver(false),
        onDrop: (e: DragEvent) => {
          if (!files(e)) return
          e.preventDefault()
          setOver(false)
          send([...e.dataTransfer.files])
        },
      }
    : undefined
  return { over: on && over, handlers }
}
