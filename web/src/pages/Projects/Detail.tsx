import { useState } from 'react'
import { api } from '../../api/client.ts'
import type { Project, Settings } from '../../api/types.ts'
import EnvCheck from '../../components/EnvCheck.tsx'
import { changes, formOf, type Form } from './edit.ts'
import Sections from './Sections.tsx'

type Props = { project: Project; folded: boolean; onProject: (p: Project) => void; onRemove: () => void }
type Editing = { was: Project; form: Form; credentials: Settings['forge_credentials'] }

// The page keys this by the project's id, so choosing another project ends an edit. A phone has no edit.
export default function Detail({ project, folded, onProject, onRemove }: Props) {
  const [edit, setEdit] = useState<Editing | null>(null)
  const [refused, setRefused] = useState('')
  const [sending, setSending] = useState(false)

  function start() {
    setRefused('')
    setEdit({ was: project, form: formOf(project), credentials: [] })
    api<Settings>('GET', '/settings').then(
      (s) => setEdit((e) => e && { ...e, credentials: s.forge_credentials }),
      () => {}, // the default stays choosable
    )
  }
  const set = <K extends keyof Form>(key: K, value: Form[K]) => setEdit((e) => e && { ...e, form: { ...e.form, [key]: value } })

  // The fields hold what was typed while reads go on; what is sent is measured against the project when Edit was pressed.
  async function save(e: Editing) {
    const body = changes(e.was, e.form)
    if (Object.keys(body).length === 0) {
      setEdit(null)
      return
    }
    setRefused('')
    setSending(true)
    let p: Project
    try {
      p = await api<Project>('PATCH', `/projects/${project.id}`, body)
    } catch (err) {
      setRefused(err instanceof Error ? err.message : String(err))
      setSending(false)
      return
    }
    setSending(false)
    setEdit(null)
    onProject(p)
    // An edit that cleared the baseline is checked again, as a new project is; a refused request leaves Check to the person.
    if (!p.runnable && p.baseline === null && p.check.requested_at === null)
      api<Project>('POST', `/projects/${project.id}/check`).then(onProject, () => {})
  }

  return (
    <>
      {!folded && (
        <div className="phead">
          <h2>{project.name}</h2>
          <span className="repo mono">{project.repo_url}</span>
          <span className="spacer" />
          {edit ? (
            <>
              {refused && (
                <p className="refused" role="alert">
                  {refused}
                </p>
              )}
              <button className="btn" onClick={() => setEdit(null)}>
                Cancel
              </button>
              <button className="btn primary" disabled={sending} onClick={() => void save(edit)}>
                Save
              </button>
            </>
          ) : (
            <button className="btn" onClick={start}>
              Edit
            </button>
          )}
        </div>
      )}
      <EnvCheck project={project} onProject={onProject} />
      <Sections project={project} folded={folded} onRemove={onRemove} edit={edit && { form: edit.form, credentials: edit.credentials, set }} />
    </>
  )
}
