import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { api } from '../../api/client.ts'
import { useStream } from '../../api/stream.ts'
import type { Project } from '../../api/types.ts'
import Dialog from '../../components/Dialog.tsx'
import PageHead from '../../components/PageHead.tsx'
import { useNarrow } from '../../components/useNarrow.ts'
import Detail from './Detail.tsx'
import NewProject from './NewProject.tsx'
import './Projects.css'

const square = (p: Project) => (p.runnable ? 'k-ready' : 'k-stop')

// The server decides whether a project can go, and its refusal is shown here as it said it.
function RemoveDialog({ project, onClose, onRemoved }: { project: Project; onClose: () => void; onRemoved: () => void }) {
  const [refused, setRefused] = useState('')
  const [sending, setSending] = useState(false)
  async function remove() {
    setRefused('')
    setSending(true)
    try {
      await api('DELETE', `/projects/${project.id}`)
      onRemoved()
    } catch (e) {
      setRefused(e instanceof Error ? e.message : String(e))
      setSending(false)
    }
  }
  return (
    <Dialog title={`Remove ${project.name}?`} onClose={onClose}>
      {refused && (
        <p className="refused" role="alert">
          {refused}
        </p>
      )}
      <div className="acts">
        <button className="btn" onClick={onClose}>
          Cancel
        </button>
        <button className="btn danger" disabled={sending} onClick={() => void remove()}>
          Remove project
        </button>
      </div>
    </Dialog>
  )
}

export default function Projects() {
  const { name } = useParams()
  const narrow = useNarrow()
  const navigate = useNavigate()
  const [projects, setProjects] = useState<Project[] | null>(null)
  // The id of the project the dialog asks to remove, so it never opens over another.
  const [removing, setRemoving] = useState(0)
  const [creating, setCreating] = useState(false)

  const read = () => {
    api<{ projects: Project[] }>('GET', '/projects')
      .then((r) => setProjects(r.projects))
      .catch(() => {}) // the next event or reconnect reads again
  }
  useStream(read)

  const list = projects ?? []
  const project = name === undefined ? list[0] : list.find((p) => p.name === name)
  const put = (p: Project) => setProjects((l) => l && l.map((x) => (x.id === p.id ? p : x)))
  const removed = () => {
    setRemoving(0)
    navigate('/projects')
    read()
  }

  const created = (made: string) => {
    setCreating(false)
    navigate(`/projects/${made}`)
    read()
  }

  const view = project && (
    <>
      <Detail key={project.id} project={project} folded={narrow} onProject={put} onRemove={() => setRemoving(project.id)} />
      {removing === project.id && <RemoveDialog project={project} onClose={() => setRemoving(0)} onRemoved={removed} />}
    </>
  )

  if (narrow && name !== undefined)
    return (
      <div className="page projects-page">
        <div className="crumb">
          <Link to="/projects">Projects</Link> / {name}
        </div>
        {project && <PageHead title={project.name} />}
        {view}
      </div>
    )
  if (narrow)
    return (
      <div className="page projects-page">
        <PageHead title="Projects" />
        <p className="sub">{projects ? `${projects.length} ${projects.length === 1 ? 'project' : 'projects'}.` : '\u00a0'}</p>
        <nav className="plist flat" aria-label="Projects">
          {list.map((p) => (
            <Link key={p.id} className={`pitem ${square(p)}`} to={`/projects/${p.name}`}>
              <i className="mk" />
              <span>
                <span className="t1">{p.name}</span>
                <span className="t2">{p.runnable ? 'Can run tasks' : 'Cannot run tasks yet'}</span>
              </span>
            </Link>
          ))}
        </nav>
      </div>
    )

  return (
    <div className="page projects-page">
      <PageHead title="Projects">
        <button className="btn primary" onClick={() => setCreating(true)}>
          New project
        </button>
      </PageHead>
      {creating && <NewProject onClose={() => setCreating(false)} onCreated={created} />}
      <p className="sub">{'\u00a0'}</p>
      <div className="panes">
        <nav className="plist" aria-label="Projects">
          {list.map((p) => (
            <Link key={p.id} className={`pitem ${square(p)}`} to={`/projects/${p.name}`} aria-current={p === project ? 'page' : undefined}>
              <i className="mk" />
              <span className="t1">{p.name}</span>
            </Link>
          ))}
        </nav>
        <div className="detail">{view}</div>
      </div>
    </div>
  )
}
