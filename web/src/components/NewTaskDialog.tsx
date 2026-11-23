import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router'
import { api } from '../api/client.ts'
import { fitting, uploadImage, useCap, useDrop } from '../api/images.ts'
import type { Project, Task } from '../api/types.ts'
import Dialog from './Dialog.tsx'
import Field from './Field.tsx'
import { DropBox, ImagesSection, Tile } from './Images.tsx'
import { useNarrow } from './useNarrow.ts'
import './NewTaskDialog.css'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

// A held image's address is made from the person's own file while its thumbnail shows, and released when it goes.
function Thumb({ file }: { file: File }) {
  const shown = useCallback(
    (img: HTMLImageElement) => {
      const url = URL.createObjectURL(file)
      img.src = url
      return () => URL.revokeObjectURL(url)
    },
    [file],
  )
  return <img ref={shown} alt="" />
}

export function NewTaskForm({ onCancel }: { onCancel?: () => void }) {
  const navigate = useNavigate()
  const narrow = useNarrow()
  const [projects, setProjects] = useState<Project[]>([])
  const [project, setProject] = useState('')
  const [title, setTitle] = useState('')
  const [body, setBody] = useState('')
  const [depends, setDepends] = useState('')
  const [start, setStart] = useState(false)
  const [error, setError] = useState('')
  const [sending, setSending] = useState(false)
  const [held, setHeld] = useState<{ key: number; file: File }[]>([])
  const [refused, setRefused] = useState('')
  const count = useRef(0)
  const { cap, ends } = useCap()

  useEffect(() => {
    api<{ projects: Project[] }>('GET', '/projects')
      .then((r) => {
        setProjects(r.projects)
        setProject((p) => p || r.projects[0]?.name || '')
      })
      .catch((e: unknown) => setError(message(e)))
  }, [])

  // Nothing is stored until the task exists.
  const hold = (files: File[]) => {
    const fit = fitting(files, cap, setRefused).map((file) => ({ key: ++count.current, file }))
    setHeld((h) => [...h, ...fit])
  }
  const drop = useDrop(!narrow && !sending, hold)

  // A task with images is created a draft, so no run starts before they are stored. Closing the form stops it all.
  async function submit(e: FormEvent) {
    e.preventDefault()
    const depends_on = (depends.match(/\d+/g) ?? []).map(Number)
    setSending(true)
    let task: Task
    try {
      task = await api<Task>('POST', '/tasks', { project, title, body, depends_on, start: start && held.length === 0 })
    } catch (err) {
      setError(message(err))
      setSending(false)
      return
    }
    const { signal } = ends.current!
    const failed: string[] = []
    // One at a time, so the task keeps them in the order they were added.
    for (const { file } of held) {
      await uploadImage(task.id, file, signal).catch((err: unknown) => failed.push(`${file.name} was not attached: ${message(err)}`))
      if (signal.aborted) return
    }
    let unstarted = ''
    if (held.length > 0 && start && failed.length === 0) {
      await api('POST', `/tasks/${task.id}/actions`, { action: 'start' }).catch((err: unknown) => (unstarted = message(err)))
    }
    if (!signal.aborted) navigate(`/tasks/${task.id}`, { state: { images: failed.join('\n'), refused: unstarted } })
  }

  return (
    <form className={drop.over ? 'new-task dropping' : 'new-task'} onSubmit={(e) => void submit(e)} {...drop.handlers}>
      {drop.over && <DropBox what="the new task" cap={cap} />}
      <Field label="Project">
        <select value={project} onChange={(e) => setProject(e.target.value)}>
          {projects.map((p) => (
            <option key={p.name}>{p.name}</option>
          ))}
        </select>
      </Field>
      <Field label="What should be done?">
        <input autoFocus required value={title} onChange={(e) => setTitle(e.target.value)} />
      </Field>
      <Field label="Details">
        <textarea placeholder="What done looks like, what to leave alone." value={body} onChange={(e) => setBody(e.target.value)} />
      </Field>
      <ImagesSection
        where={onCancel ? 'dialog' : 'page'}
        tiles={
          sending
            ? []
            : held.map((h) => (
                <Tile key={h.key} name={h.file.name} onRemove={() => setHeld(held.filter((x) => x !== h))}>
                  <Thumb file={h.file} />
                </Tile>
              ))
        }
        going={sending ? held.map((h) => ({ key: h.key, name: h.file.name, size: h.file.size })) : []}
        refused={refused}
        cap={cap}
        onAdd={sending ? undefined : hold}
      />
      <Field label="Depends on">
        <input placeholder="Task numbers, such as 42" value={depends} onChange={(e) => setDepends(e.target.value)} />
      </Field>
      <Field label="Start it now" check>
        <input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} />
      </Field>
      <p className="new-task-hint">{start ? 'It is started when you create it.' : 'It is kept as a draft until you start it.'}</p>
      {error && (
        <p role="alert" className="new-task-error">
          {error}
        </p>
      )}
      <div className={onCancel ? 'acts' : 'new-task-create'}>
        {onCancel && (
          <button type="button" className="btn" onClick={onCancel}>
            Cancel
          </button>
        )}
        <button className="btn primary" disabled={sending}>
          Create
        </button>
      </div>
    </form>
  )
}

export function NewTaskDialog({ onClose }: { onClose: () => void }) {
  return (
    <Dialog title="New task" onClose={onClose}>
      <NewTaskForm onCancel={onClose} />
    </Dialog>
  )
}
