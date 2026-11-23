import { useEffect, useRef, useState } from 'react'
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

export const imageURL = (task: number, image: number) => `/api/tasks/${task}/images/${image}`

export const uploadImage = (task: number, f: File, signal?: AbortSignal) =>
  api<Image>('POST', `/tasks/${task}/images?${new URLSearchParams({ name: f.name })}`, f, { signal })

export const removeImage = (task: number, image: number) => api<Task>('PATCH', `/tasks/${task}`, { remove_images: [image] })

type Upload = { key: number; name: string; size: number }

// refused is the line of the last upload or removal refused, which refuse sets.
export type Uploads = { cap: number; going: Upload[]; refused: string; refuse: (line: string) => void; send: (files: File[]) => void }

// Attaches images to the task with the id, each under way until done after it ends. A file the server would refuse is
// refused before it is sent, and an upload still under way ends when the page is left.
export function useUploads(id: number, done: () => Promise<unknown>): Uploads {
  const [cap, setCap] = useState(0)
  const [going, setGoing] = useState<Upload[]>([])
  const [refused, setRefused] = useState('')
  const ends = useRef<AbortController>(null)
  const count = useRef(0)
  // Made in the effect, so the second run of a development build has its own.
  useEffect(() => {
    const c = new AbortController()
    ends.current = c
    api<Settings>('GET', '/settings', undefined, { signal: c.signal }).then((s) => setCap(s.image_max_bytes), () => {})
    return () => c.abort()
  }, [])

  const send = (files: File[]) => {
    const { signal } = ends.current!
    setRefused('')
    for (const f of files) {
      const no = refusal(f, cap)
      if (no) {
        setRefused(no)
        continue
      }
      const u = { key: ++count.current, name: f.name, size: f.size }
      setGoing((g) => [...g, u])
      uploadImage(id, f, signal)
        .then(done, (e: unknown) => !signal.aborted && setRefused(e instanceof Error ? e.message : String(e)))
        .finally(() => setGoing((g) => g.filter((x) => x !== u)))
    }
  }
  return { cap, going, refused, refuse: setRefused, send }
}
