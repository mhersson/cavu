import { useEffect, useRef, useState } from 'react'
import { Link } from 'react-router'
import { api } from '../../api/client.ts'
import { useStream } from '../../api/stream.ts'
import type { Needs, Row, Task } from '../../api/types.ts'
import NeedsBox from '../../components/NeedsBox.tsx'
import { NewTaskDialog } from '../../components/NewTaskDialog.tsx'
import PageHead from '../../components/PageHead.tsx'
import StatusMark from '../../components/StatusMark.tsx'
import { ago, clock } from '../../components/time.ts'
import { useNarrow } from '../../components/useNarrow.ts'
import './NeedsYou.css'

type Lists = { needing: Row[]; working: Row[] }

const kinds: Record<Needs, string> = {
  '': '',
  answer: 'Question',
  retry: 'Stopped',
  review: 'Ready for review',
  take: 'Change to take',
}

// A row that needs a person has the time it began to.
const since = (r: Row) => r.needs_you_since!
const kind = (r: Row) =>
  kinds[r.needs] + (r.needs === 'retry' ? ', ' + r.stop_reason.replace(/_/g, ' ') : '') + ', ' + ago(since(r))
const tasks = (n: number, one: string, many: string) => (n === 1 ? `1 task ${one}` : `${n} tasks ${many}`)

export default function NeedsYou() {
  const narrow = useNarrow()
  const [lists, setLists] = useState<Lists | null>(null)
  const [views, setViews] = useState(new Map<number, Task>())
  const [chosen, setChosen] = useState(0)
  const [creating, setCreating] = useState(false)

  const needing = lists?.needing ?? []
  const working = lists?.working ?? []
  const shown = narrow ? needing.map((r) => r.id) : chosen ? [chosen] : []

  const keep = (t: Task) => setViews((m) => new Map(m).set(t.id, t))
  const read = (id: number) => {
    api<Task>('GET', `/tasks/${id}`)
      .then(keep)
      .catch(() => {}) // the next event or reconnect reads again
  }

  const readLists = () => {
    Promise.all([
      api<{ tasks: Row[] }>('GET', '/tasks?needs_you=true'),
      api<{ tasks: Row[] }>('GET', '/tasks?status=queued,running,blocked,in_review'),
    ])
      .then(([n, w]) => {
        const rows = n.tasks.toSorted((a, b) => Date.parse(since(b)) - Date.parse(since(a)))
        setLists({ needing: rows, working: w.tasks.filter((r) => !r.needs_you) })
        // The choice holds while its task is listed, so a task that comes in does not take it away.
        setChosen((c) => (rows.some((r) => r.id === c) ? c : (rows[0]?.id ?? 0)))
      })
      .catch(() => {})
  }
  // A task that no longer needs a person leaves the list now, not at the next burst of events.
  const acted = (t: Task) => {
    keep(t)
    if (!t.needs) readLists()
  }

  useStream((ids) => {
    readLists()
    for (const id of shown) if (!ids || ids.has(id)) read(id)
  })

  // A task is read as it comes into view, and after that when an event names it.
  const inView = useRef(new Set<number>())
  useEffect(() => {
    for (const id of shown) if (!inView.current.has(id)) read(id)
    inView.current = new Set(shown)
  })

  // How long ago is worked out as the page renders, so a quiet page renders each minute.
  const [, setMinute] = useState(0)
  useEffect(() => {
    const timer = setInterval(() => setMinute((m) => m + 1), 60000)
    return () => clearInterval(timer)
  }, [])

  const n = needing.length
  const empty = lists !== null && n === 0
  const sub = n ? tasks(n, 'waits', 'wait') + ' for you' + (narrow ? '.' : ', the most recent first.') : '\u00a0'
  const nobody = tasks(working.length, 'is', 'are') + ' working without you.'

  if (narrow) {
    return (
      <div className="page needs-you">
        <PageHead title="Needs you" />
        <p className="sub">{sub}</p>
        {empty ? (
          <div className="empty">
            <b>Nothing needs you.</b>
            {nobody}
          </div>
        ) : (
          needing.map((r) => {
            const t = views.get(r.id)
            return t && <NeedsBox key={r.id} task={t} onTask={acted} />
          })
        )}
      </div>
    )
  }

  const task = views.get(chosen)
  return (
    <div className="page needs-you">
      <PageHead title="Needs you">
        <button className="btn primary" onClick={() => setCreating(true)}>
          New task
        </button>
      </PageHead>
      <p className="sub">{sub}</p>
      {empty && (
        <>
          <div className="empty">
            <b>Nothing needs you.</b>
            {nobody} This page fills when a task needs you to answer it, retry it, review its pull request or take
            its change.
          </div>
          {working.length > 0 && (
            <>
              <h2 className="sec">Working without you</h2>
              <p className="sub">{tasks(working.length, 'that needs', 'that need')} nothing from you now.</p>
              <table className="rows">
                <tbody>
                  {working.map((r) => (
                    <tr key={r.id}>
                      <td className="id">{r.id}</td>
                      <td className="proj">{r.project}</td>
                      <td>
                        <Link to={`/tasks/${r.id}`}>{r.title}</Link>
                      </td>
                      <td>
                        <StatusMark status={r.status} />
                      </td>
                      <td className="wait">{r.waiting}</td>
                      <td className="until">{r.waiting_until && `until ${clock(r.waiting_until)}`}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </>
          )}
        </>
      )}
      {n > 0 && (
        <div className="panes">
          <div className="plist">
            {needing.map((r) => (
              <button
                key={r.id}
                className={`pitem k-${r.needs}`}
                aria-current={r.id === chosen || undefined}
                onClick={() => setChosen(r.id)}
              >
                <i className="mk" />
                <span>
                  <span className="t1">{r.title}</span>
                  <span className="t2">{kind(r)}</span>
                </span>
              </button>
            ))}
            {working.length > 0 && <h3>Working without you</h3>}
            {working.map((r) => (
              <Link key={r.id} className="pitem" to={`/tasks/${r.id}`}>
                <span />
                <span>
                  <span className="t1">{r.title}</span>
                  <span className="t2">
                    <StatusMark status={r.status} /> {r.waiting}
                  </span>
                </span>
              </Link>
            ))}
          </div>
          <div className="detail">{task && <NeedsBox flat task={task} onTask={acted} key={task.id} />}</div>
        </div>
      )}
      {creating && <NewTaskDialog onClose={() => setCreating(false)} />}
    </div>
  )
}
