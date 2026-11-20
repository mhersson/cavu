import { useState, type ReactNode } from 'react'
import { Link, useParams } from 'react-router'
import { api } from '../../api/client.ts'
import { useStream } from '../../api/stream.ts'
import type { Task as TaskView } from '../../api/types.ts'
import Dialog from '../../components/Dialog.tsx'
import { ChangeFacts, PullRequestFacts, ReviewFacts } from '../../components/Facts.tsx'
import NeedsBox from '../../components/NeedsBox.tsx'
import PageHead from '../../components/PageHead.tsx'
import StatusMark from '../../components/StatusMark.tsx'
import { clock, duration } from '../../components/time.ts'
import { useNarrow } from '../../components/useNarrow.ts'
import './Task.css'

const usd = (n: number) => '$' + n.toFixed(2)
const num = (n: number) => (n >= 1e6 ? (n / 1e6).toFixed(2) + ' M' : n >= 1e3 ? Math.round(n / 1e3) + ' k' : String(n))

export default function Task() {
  const { id } = useParams()
  // A link from one task to another remounts the page, so its first read is the new task's.
  return <Page key={id} id={Number(id)} />
}

function Meter({ label, now, max }: { label: string; now: number; max: number }) {
  return (
    <div className="meter" role="meter" aria-label={label} aria-valuemin={0} aria-valuenow={now} aria-valuemax={max}>
      <i style={{ width: `${Math.min(100, (100 * now) / max)}%` }} />
    </div>
  )
}

function Cost({ task }: { task: TaskView }) {
  return (
    <section>
      <h3>Cost and use</h3>
      <dl className="kv">
        <dt>Estimate</dt>
        <dd>
          {usd(task.cost_estimate_usd)} <span className="small">from list prices, not a bill</span>
        </dd>
        <dt>Time</dt>
        <dd>{duration(task.duration_ms)}</dd>
      </dl>
      {task.cost_missing_sessions > 0 && (
        <p className="small">The estimate leaves out {task.cost_missing_sessions} sessions without a cost.</p>
      )}
      {task.usage.length > 0 && (
        <table className="usage">
          <thead>
            <tr>
              <th>Model</th>
              <th>In</th>
              <th>Out</th>
              <th>Cache read</th>
              <th>Cache write</th>
            </tr>
          </thead>
          <tbody>
            {task.usage.map((u) => (
              <tr key={u.source + u.model}>
                <td>{u.model.replace('claude-', '')}</td>
                <td>{num(u.input)}</td>
                <td>{num(u.output)}</td>
                <td>{num(u.cache_read)}</td>
                <td>{num(u.cache_write)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}

function Budget({ task }: { task: TaskView }) {
  const b = task.budget
  return (
    <section>
      <h3>Since you last acted</h3>
      <div className="small">
        {b.turns} of {b.max_turns} turns
      </div>
      <Meter label="Turns" now={b.turns} max={b.max_turns} />
      <div className="small">
        {b.minutes} of {b.max_minutes} minutes
      </div>
      <Meter label="Minutes" now={b.minutes} max={b.max_minutes} />
      {b.usd !== undefined && b.max_usd !== undefined && (
        <>
          <div className="small">
            {usd(b.usd)} of {usd(b.max_usd)}
          </div>
          <Meter label="Money" now={b.usd} max={b.max_usd} />
        </>
      )}
      {b.day_usd !== undefined && b.max_day_usd !== undefined && (
        <>
          <div className="small">
            {usd(b.day_usd)} of {usd(b.max_day_usd)} today
          </div>
          <Meter label="Money today" now={b.day_usd} max={b.max_day_usd} />
        </>
      )}
    </section>
  )
}

function Page({ id }: { id: number }) {
  const narrow = useNarrow()
  const [task, setTask] = useState<TaskView | null>(null)
  const [missing, setMissing] = useState('')
  const [asking, setAsking] = useState(false)
  const [refused, setRefused] = useState('')

  const read = () =>
    api<TaskView>('GET', `/tasks/${id}`).then(
      (t) => {
        setTask(t)
        setMissing('')
      },
      (e: Error) => setMissing(e.message), // only shown while there is no task
    )

  useStream((ids) => {
    if (!ids || ids.has(id)) void read()
  })

  if (!task) return <div className="page task-page">{missing && <p>{missing}</p>}</div>

  async function cancel() {
    setAsking(false)
    try {
      setTask(await api<TaskView>('POST', `/tasks/${id}/actions`, { action: 'cancel' }))
      setRefused('')
    } catch (e) {
      setRefused(e instanceof Error ? e.message : String(e))
      await read()
    }
  }

  const open = task.status !== 'done' && task.status !== 'cancelled'
  const button = (
    <button className="btn danger" onClick={() => setAsking(true)}>
      Cancel task
    </button>
  )
  const cost = usd(task.cost_estimate_usd) + ' estimated'
  const fold = (title: string, has: unknown, facts: ReactNode) =>
    has ? (
      <details className="acc">
        <summary>{title}</summary>
        {facts}
      </details>
    ) : null

  return (
    <div className="page task-page">
      <div className="crumb">
        <Link to="/">Needs you</Link> / {task.project} / task {task.id}
      </div>
      <PageHead title={task.title}>{!narrow && open && button}</PageHead>
      {refused && <p role="alert" className="refused">{refused}</p>}
      <div className="task-meta">
        <StatusMark status={task.status} />
        {!narrow && (
          <span>
            filed over {task.requester} at {clock(task.created_at)}
          </span>
        )}
        <span>{task.turns} turns</span>
        <span>{cost}</span>
      </div>
      {task.body && <p className="task-body">{task.body}</p>}
      <NeedsBox task={task} onTask={(t) => setTask(t)} onPage />
      {!task.needs_you && task.waiting && (
        <p className="waitline">
          {task.waiting}
          {task.waiting_until && ` Until ${clock(task.waiting_until)}.`}
        </p>
      )}
      {narrow ? (
        <>
          {fold('Pull request', task.pull_request, <PullRequestFacts pr={task.pull_request} />)}
          {fold('Change', task.change, <ChangeFacts change={task.change} />)}
          {fold('Review', task.review, <ReviewFacts review={task.review} />)}
          {fold('Cost and use', true, <Cost task={task} />)}
          <h2 className="sec">What happened</h2>
          {open && <p className="cancel-row">{button}</p>}
        </>
      ) : (
        <div className="task-grid">
          <div>
            <h2 className="sec">What happened</h2>
          </div>
          <aside className="rail">
            <PullRequestFacts pr={task.pull_request} />
            <ChangeFacts change={task.change} />
            <ReviewFacts review={task.review} />
            <Cost task={task} />
            <Budget task={task} />
            {task.dependencies.length > 0 && (
              <section>
                <h3>Depends on</h3>
                {task.dependencies.map((d) => (
                  <div key={d.id}>
                    <Link to={`/tasks/${d.id}`}>Task {d.id}</Link> <StatusMark status={d.status} />
                  </div>
                ))}
              </section>
            )}
          </aside>
        </div>
      )}
      {asking && (
        <Dialog title="Cancel this task?" onClose={() => setAsking(false)}>
          <div className="acts">
            <button className="btn" onClick={() => setAsking(false)}>
              Keep the task
            </button>
            <button className="btn danger" onClick={() => void cancel()}>
              Cancel the task
            </button>
          </div>
        </Dialog>
      )}
    </div>
  )
}
