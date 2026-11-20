import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router'
import { api } from '../api/client.ts'
import type { Project, Task } from '../api/types.ts'
import Dialog from './Dialog.tsx'
import Field from './Field.tsx'
import './NewTaskDialog.css'

export function NewTaskForm({ onCancel }: { onCancel?: () => void }) {
  const navigate = useNavigate()
  const [projects, setProjects] = useState<Project[]>([])
  const [project, setProject] = useState('')
  const [title, setTitle] = useState('')
  const [body, setBody] = useState('')
  const [depends, setDepends] = useState('')
  const [start, setStart] = useState(false)
  const [error, setError] = useState('')
  const [sending, setSending] = useState(false)

  useEffect(() => {
    api<{ projects: Project[] }>('GET', '/projects')
      .then((r) => {
        setProjects(r.projects)
        setProject((p) => p || r.projects[0]?.name || '')
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
  }, [])

  async function submit(e: FormEvent) {
    e.preventDefault()
    const depends_on = (depends.match(/\d+/g) ?? []).map(Number)
    setSending(true)
    try {
      const task = await api<Task>('POST', '/tasks', { project, title, body, depends_on, start })
      navigate(`/tasks/${task.id}`)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
      setSending(false)
    }
  }

  return (
    <form className="new-task" onSubmit={(e) => void submit(e)}>
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
        <button className="btn primary" disabled={sending}>Create</button>
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
