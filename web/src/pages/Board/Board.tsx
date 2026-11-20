import { useEffect, useState } from 'react'
import { Link, useSearchParams } from 'react-router'
import { api } from '../../api/client.ts'
import { useStream } from '../../api/stream.ts'
import type { Project, Row, Status } from '../../api/types.ts'
import { NewTaskDialog } from '../../components/NewTaskDialog.tsx'
import PageHead from '../../components/PageHead.tsx'
import StatusMark from '../../components/StatusMark.tsx'
import { clock } from '../../components/time.ts'
import './Board.css'

const open: Status[] = ['draft', 'queued', 'running', 'blocked', 'in_review']
const finishedLimit = 30

export default function Board() {
  const [params, setParams] = useSearchParams()
  const [projects, setProjects] = useState<Project[]>([])
  const [creating, setCreating] = useState(false)
  const finished = params.get('finished') === '1'
  const project = params.get('project') ?? ''

  useEffect(() => {
    api<{ projects: Project[] }>('GET', '/projects')
      .then((r) => setProjects(r.projects))
      .catch(() => {})
  }, [])

  const change = (key: string, value: string) =>
    setParams((p) => {
      const next = new URLSearchParams(p)
      if (value) next.set(key, value)
      else next.delete(key)
      return next
    })

  return (
    <div className="page board-page">
      <PageHead title="Board">
        <div className="tools">
          <label className="inline">
            <input type="checkbox" checked={finished} onChange={(e) => change('finished', e.target.checked ? '1' : '')} />
            Done and cancelled
          </label>
          <select className="in" aria-label="Project" value={project} onChange={(e) => change('project', e.target.value)}>
            <option value="">Every project</option>
            {projects.map((p) => (
              <option key={p.id}>{p.name}</option>
            ))}
          </select>
          <button className="btn primary" onClick={() => setCreating(true)}>
            New task
          </button>
        </div>
      </PageHead>
      {/* Keyed on the query, so a change of tick or project subscribes, and so reads, afresh. */}
      <Columns key={`${finished}/${project}`} finished={finished} project={project} />
      {creating && <NewTaskDialog onClose={() => setCreating(false)} />}
    </div>
  )
}

function Columns({ finished, project }: { finished: boolean; project: string }) {
  const [rows, setRows] = useState<Row[] | null>(null)
  const columns: Status[] = finished ? [...open, 'done', 'cancelled'] : open

  useStream(() => {
    const scope = project ? '&project=' + encodeURIComponent(project) : ''
    const tail = `&order=newest&limit=${finishedLimit}` + scope
    const paths = ['/tasks?status=' + open.join(',') + scope]
    if (finished) paths.push('/tasks?status=done' + tail, '/tasks?status=cancelled' + tail)
    Promise.all(paths.map((p) => api<{ tasks: Row[] }>('GET', p)))
      .then((lists) => setRows(lists.flatMap((l) => l.tasks)))
      .catch(() => {}) // the next event or reconnect reads again
  })

  const shown = rows ?? []
  return (
    <>
      <p className="sub">
        {rows ? `${rows.length} ${rows.length === 1 ? 'task' : 'tasks'}, each where CAVU has it now. Nothing here is dragged.` : '\u00a0'}
      </p>
      <div className="board" style={{ gridTemplateColumns: `repeat(${columns.length}, minmax(0, 1fr))` }}>
        {columns.map((status) => {
          const here = shown.filter((r) => r.status === status)
          return (
            <section className="bcol" key={status} aria-label={status}>
              <h2>
                <StatusMark status={status} />
                <span className="n">{rows && here.length}</span>
              </h2>
              {here.length === 0 && rows && <div className="none">None</div>}
              {here.map((r) => (
                <Link className="bitem" key={r.id} to={`/tasks/${r.id}`}>
                  <div className="b1">
                    <span className="num">{r.id}</span>
                    <span className="proj">{r.project}</span>
                    {r.needs_you && <span className="mark">needs you</span>}
                  </div>
                  <div className="b2">{r.title}</div>
                  {r.waiting && (
                    <div className="b3">
                      {r.waiting}
                      {r.waiting_until && ` Until ${clock(r.waiting_until)}.`}
                    </div>
                  )}
                </Link>
              ))}
            </section>
          )
        })}
      </div>
    </>
  )
}
