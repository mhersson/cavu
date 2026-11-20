import { useState } from 'react'
import { Link } from 'react-router'
import { api } from '../../api/client.ts'
import { useStream } from '../../api/stream.ts'
import type { Row, Status as TaskStatus } from '../../api/types.ts'
import PageHead from '../../components/PageHead.tsx'
import StatusMark from '../../components/StatusMark.tsx'
import './Status.css'

const groups: TaskStatus[] = ['running', 'queued', 'blocked', 'in_review', 'draft']

export default function Status() {
  const [rows, setRows] = useState<Row[] | null>(null)

  useStream(() => {
    api<{ tasks: Row[] }>('GET', '/tasks?status=draft,queued,running,blocked,in_review')
      .then((r) => setRows(r.tasks))
      .catch(() => {}) // the next event or reconnect reads again
  })

  return (
    <div className="page status-page">
      <PageHead title="Status" />
      <p className="sub">{rows ? `${rows.length} open ${rows.length === 1 ? 'task' : 'tasks'}.` : '\u00a0'}</p>
      {groups.map((status) => {
        const here = (rows ?? []).filter((r) => r.status === status)
        if (here.length === 0) return null
        return (
          <section key={status} aria-label={status}>
            <h2 className="grp">
              <StatusMark status={status} />
              <span>{here.length}</span>
            </h2>
            {here.map((r) => (
              <Link className="prow" key={r.id} to={`/tasks/${r.id}`}>
                <span className="id">{r.id}</span>
                <div>
                  <div>{r.title}</div>
                  {(r.needs_you || r.waiting) && (
                    <div className="w">
                      {r.needs_you && 'Needs you. '}
                      {r.waiting}
                    </div>
                  )}
                </div>
              </Link>
            ))}
          </section>
        )
      })}
    </div>
  )
}
