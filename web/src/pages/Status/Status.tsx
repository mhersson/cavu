import { useState } from 'react'
import { Link, useSearchParams } from 'react-router'
import { useStream } from '../../api/stream.ts'
import type { Row, Status as TaskStatus } from '../../api/types.ts'
import { count, open, readLists } from '../../components/lists.ts'
import PageHead from '../../components/PageHead.tsx'
import StatusMark from '../../components/StatusMark.tsx'
import './Status.css'

const groups: TaskStatus[] = ['running', 'queued', 'blocked', 'in_review', 'draft']

export default function Status() {
  const [params, setParams] = useSearchParams()
  const finished = params.get('finished') === '1'

  const tick = (on: boolean) =>
    setParams((p) => {
      const next = new URLSearchParams(p)
      if (on) next.set('finished', '1')
      else next.delete('finished')
      return next
    })

  return (
    <div className="page status-page">
      <PageHead title="Status">
        <label className="inline">
          <input type="checkbox" checked={finished} onChange={(e) => tick(e.target.checked)} />
          Done and cancelled
        </label>
      </PageHead>
      {/* Keyed on the tick, so a change of it subscribes, and so reads, afresh. */}
      <Groups key={String(finished)} finished={finished} />
    </div>
  )
}

function Groups({ finished }: { finished: boolean }) {
  const [rows, setRows] = useState<Row[] | null>(null)
  const shownGroups: TaskStatus[] = finished ? [...groups, 'done', 'cancelled'] : groups

  useStream(() => {
    readLists(finished)
      .then(setRows)
      .catch(() => {}) // the next event or reconnect reads again
  })

  const openCount = (rows ?? []).filter((r) => open.includes(r.status)).length
  return (
    <>
      <p className="sub">{rows ? `${openCount} open ${openCount === 1 ? 'task' : 'tasks'}.` : '\u00a0'}</p>
      {shownGroups.map((status) => {
        const here = (rows ?? []).filter((r) => r.status === status)
        if (here.length === 0) return null
        return (
          <section key={status} aria-label={status}>
            <h2 className="grp">
              <StatusMark status={status} />
              <span>{count(status, here.length)}</span>
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
    </>
  )
}
