import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { Link, useNavigationType, useSearchParams } from 'react-router'
import { api, ApiError } from '../../api/client.ts'
import { useStream } from '../../api/stream.ts'
import type { Needs, Project, Row, Status } from '../../api/types.ts'
import { count, finishedLimit, readLists } from '../../components/lists.ts'
import { NewTaskDialog } from '../../components/NewTaskDialog.tsx'
import PageHead from '../../components/PageHead.tsx'
import StatusMark from '../../components/StatusMark.tsx'
import { clock } from '../../components/time.ts'
import { useNarrow } from '../../components/useNarrow.ts'
import { XIcon } from '../../components/icons.tsx'
import BoardIcon from './BoardIcon.tsx'
import './Board.css'

const statuses: Status[] = ['running', 'queued', 'blocked', 'in_review', 'draft', 'done', 'cancelled']
const actions: Record<Needs, string> = {
  '': 'Needs you',
  answer: 'Answer a question',
  retry: 'Retry the task',
  review: 'Review the pull request',
  take: 'Take the change',
}

export default function Board({ title = 'Board' }: { title?: 'Board' | 'Status' }) {
  const narrow = useNarrow()
  const [params, setParams] = useSearchParams()
  const intended = useRef(params)
  const navigation = useNavigationType()
  const [projects, setProjects] = useState<Project[]>([])
  const [creating, setCreating] = useState(false)
  const input = useRef<HTMLInputElement>(null)
  const finished = params.get('finished') === '1'
  const project = params.get('project') ?? ''
  const search = params.get('q') ?? ''

  useLayoutEffect(() => {
    intended.current = params
  }, [params])

  // While typing, the field can be ahead of the router. Other navigations restore the search from the address.
  useEffect(() => {
    const field = input.current
    if (field && (navigation !== 'REPLACE' || document.activeElement !== field)) field.value = search
  }, [navigation, search])

  useEffect(() => {
    api<{ projects: Project[] }>('GET', '/projects')
      .then((r) => setProjects(r.projects))
      .catch(() => {})
  }, [])

  // Several controls can change before the router commits the first navigation.
  const change = (values: Partial<Record<'q' | 'finished' | 'project', string>>, replace = false) => {
    const next = new URLSearchParams(intended.current)
    for (const [key, value] of Object.entries(values)) {
      if (value) next.set(key, value)
      else next.delete(key)
    }
    intended.current = next
    setParams(next, { replace })
  }
  const toggleFinished = () => change({ finished: intended.current.get('finished') === '1' ? '' : '1' })
  const resetInput = () => {
    if (input.current) {
      input.current.value = ''
      input.current.focus()
    }
  }
  const clearSearch = () => {
    change({ q: '' }, true)
    resetInput()
  }
  const clearFilters = () => {
    change({ q: '', project: '', finished: '' })
    resetInput()
  }

  const picker = (
    <label className="board-control project-picker">
      <BoardIcon name="project" />
      <span aria-hidden="true">{project || 'All projects'}</span>
      <BoardIcon name="chevron" />
      <select aria-label="Project" value={project} onChange={(e) => change({ project: e.target.value })}>
        <option value="">All projects</option>
        {project && !projects.some((p) => p.name === project) && <option>{project}</option>}
        {projects.map((p) => <option key={p.id}>{p.name}</option>)}
      </select>
    </label>
  )

  return (
    <div className="page board-page">
      <PageHead title={title}>
        {narrow ? picker : <button className="btn primary" onClick={() => setCreating(true)}>New task</button>}
      </PageHead>
      <div className="board-filters">
        <div className="board-control board-search">
          <BoardIcon name="search" />
          <input
            ref={input}
            type="search"
            aria-label="Search by title or task ID"
            placeholder="Search by title or #ID"
            enterKeyHint="search"
            defaultValue={search}
            onChange={(e) => change({ q: e.target.value }, true)}
            onKeyDown={(e) => { if (e.key === 'Escape' && e.currentTarget.value) clearSearch() }}
          />
          {search && (
            <button type="button" aria-label="Clear search" onClick={clearSearch}><XIcon /></button>
          )}
        </div>
        {!narrow && (
          <>
            {picker}
            <label className="board-checkbox">
              <input type="checkbox" checked={finished} onChange={toggleFinished} /> Done and cancelled
            </label>
          </>
        )}
      </div>
      {narrow && (
        <button className="board-finished" aria-pressed={finished} aria-controls="board-list" onClick={toggleFinished}>
          <BoardIcon name="done" />Done and cancelled
        </button>
      )}
      <Groups key={`${finished}/${project}`} project={project} finished={finished} search={search} clearFilters={clearFilters} />
      {creating && <NewTaskDialog onClose={() => setCreating(false)} />}
    </div>
  )
}

const normalize = (s: string) => s.toLowerCase().replaceAll('’', "'")

function Groups({ project, finished, search, clearFilters }: {
  project: string
  finished: boolean
  search: string
  clearFilters: () => void
}) {
  const [{ rows, error }, setResult] = useState<{ read: number; rows: Row[] | null; error: string }>({
    read: 0, rows: null, error: '',
  })
  const sequence = useRef(0)
  useStream(() => {
    const read = ++sequence.current
    // A slower read may still be the freshest result available while its successor is in flight.
    const apply = (tasks: Row[] | null, message = '') => {
      setResult((current) => read > current.read ? { read, rows: tasks, error: message } : current)
    }
    readLists(finished, project)
      .then((tasks) => apply(tasks))
      .catch((e: unknown) => {
        if (project && e instanceof ApiError && e.status === 400) apply(null, e.message)
      })
  })

  const query = normalize(search.trim())
  const exactId = /^#\d+$/.test(query)
  const shown = (rows ?? []).filter((r) => {
    if (!query) return true
    if (exactId) return query === '#' + r.id
    return query === String(r.id) || normalize(r.title).includes(query)
  })
  const n = shown.length
  const unit = query ? (n === 1 ? 'match' : 'matches') : (finished ? '' : 'open ') + (n === 1 ? 'task' : 'tasks')
  const projects = new Set(shown.map((r) => r.project)).size
  const scope = project ? ' in ' + project : projects ? ` across ${projects} ${projects === 1 ? 'project' : 'projects'}` : ''
  const limitedHistory = finished && query && ['done', 'cancelled'].some(
    (status) => rows?.filter((r) => r.status === status).length === finishedLimit,
  )

  return (
    <>
      <p className="sub board-count" role="status">
        {rows ? <>{n} {unit}<span>{scope}</span></> : error || 'Loading tasks…'}
      </p>
      <div id="board-list">
        {limitedHistory && <p className="board-limit" role="note">Finished history is limited to the newest {finishedLimit} done and {finishedLimit} cancelled tasks.</p>}
        {(error || (rows && !n)) && (
          <div className="board-empty">
            {!error && <strong>{query || project ? 'No matching tasks' : finished ? 'No tasks yet' : 'No open tasks'}</strong>}
            {(query || project) && <button className="btn" onClick={clearFilters}>Clear filters</button>}
          </div>
        )}
        {statuses.map((status) => {
          const group = shown.filter((r) => r.status === status)
          return group.length > 0 && (
            <TaskGroup key={status + '/' + query} status={status} rows={group} showProject={!project} />
          )
        })}
      </div>
    </>
  )
}

function TaskGroup({ status, rows, showProject }: { status: Status; rows: Row[]; showProject: boolean }) {
  const [collapsed, setCollapsed] = useState(false)
  return (
    <section className="board-group" aria-label={status}>
      <h2>
        <button aria-expanded={!collapsed} aria-controls={'rows-' + status} onClick={() => setCollapsed(!collapsed)}>
          <StatusMark status={status}><BoardIcon name={status} /></StatusMark>
          <span className="group-count">{count(status, rows.length)}</span>
          <BoardIcon name="chevron" />
        </button>
      </h2>
      <div id={'rows-' + status} hidden={collapsed}>
        {rows.map((r) => <TaskRow key={r.id} row={r} showProject={showProject} />)}
      </div>
    </section>
  )
}

function TaskRow({ row: r, showProject }: { row: Row; showProject: boolean }) {
  let action = actions[r.needs]
  if (r.needs === 'retry' && r.stop_reason) {
    if (r.stop_reason === 'ci_failed') action = 'Retry after failed CI'
    else if (r.stop_reason === 'verify_failed') action = 'Retry after failed verification'
    else action += ` (${r.stop_reason.replaceAll('_', ' ')})`
  }
  return (
    <Link className="board-row" to={`/tasks/${r.id}`}>
      <span className="row-id">#{r.id}</span>
      <span className="row-title">{r.title}</span>
      {showProject && <span className="row-project"><BoardIcon name="project" />{r.project}</span>}
      {(r.needs_you || r.waiting || r.waiting_until) && (
        <span className="row-notes">
          {r.needs_you && (
            <span className="row-action"><BoardIcon name="person" />{action}</span>
          )}
          {(r.waiting || r.waiting_until) && (
            <span className="row-wait"><BoardIcon name="queued" />
              <span>{r.waiting}{r.waiting_until && ` Until ${clock(r.waiting_until)}.`}</span>
            </span>
          )}
        </span>
      )}
      <BoardIcon name="chevron" />
    </Link>
  )
}
