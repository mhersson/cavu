import { useRef, useState, type ReactNode } from 'react'
import { Link, useParams } from 'react-router'
import { api } from '../../api/client.ts'
import { useStream } from '../../api/stream.ts'
import type { TaskEvent, Task as TaskView } from '../../api/types.ts'
import Dialog from '../../components/Dialog.tsx'
import { ChangeFacts, PullRequestFacts, ReviewFacts } from '../../components/Facts.tsx'
import Field from '../../components/Field.tsx'
import NeedsBox from '../../components/NeedsBox.tsx'
import PageHead from '../../components/PageHead.tsx'
import StatusMark from '../../components/StatusMark.tsx'
import Timeline from '../../components/Timeline.tsx'
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
        <p className="small">The estimate leaves out {task.cost_missing_sessions} {task.cost_missing_sessions === 1 ? 'session' : 'sessions'} without a cost.</p>
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

type Editing = { was: TaskView; title: string; body: string; depends: string }

// Rows cannot see wrapped lines: grow the box to show all it holds, and never shrink it, so a drag taller stays.
function grow(el: HTMLTextAreaElement | null) {
  if (el && el.scrollHeight > el.clientHeight) el.style.height = `${el.offsetHeight + el.scrollHeight - el.clientHeight}px`
}

function Page({ id }: { id: number }) {
  const narrow = useNarrow()
  const [task, setTask] = useState<TaskView | null>(null)
  const [missing, setMissing] = useState('')
  const [asking, setAsking] = useState(false)
  const [refused, setRefused] = useState('')
  const [edit, setEdit] = useState<Editing | null>(null)
  const [sending, setSending] = useState(false)
  const [events, setEvents] = useState<TaskEvent[]>([])
  const [stretches, setStretches] = useState<ReadonlySet<number>>(() => new Set())
  const asked = useRef(new Set<number>())

  const read = () =>
    api<TaskView>('GET', `/tasks/${id}`).then(
      (t) => {
        setTask(t)
        setMissing('')
      },
      (e: Error) => setMissing(e.message), // only shown while there is no task
    )

  // Reads on while a page comes back full and its last id is below end.
  async function walk(query: Record<string, string>, size: number, end = Infinity) {
    const q = new URLSearchParams(query)
    const got: TaskEvent[] = []
    for (;;) {
      const page = (await api<{ events: TaskEvent[] }>('GET', `/tasks/${id}/events?${q}`)).events
      got.push(...page)
      if (page.length < size || page[size - 1].id >= end) return got
      q.set('after', String(page[size - 1].id))
    }
  }

  // Reads may overlap: the list keeps each event once, in id order.
  const add = (more: TaskEvent[]) =>
    setEvents((list) => [...new Map([...list, ...more].map((e) => [e.id, e])).values()].sort((a, b) => a.id - b.id))

  useStream((ids) => {
    if (ids && !ids.has(id)) return
    void read()
    // Events never change and ids only rise, so only those after the newest can be new.
    // A failed read is made good by the next burst or reconnect, which reads from the same newest.
    const newest = events.at(-1)?.id
    walk(newest === undefined ? { agent: 'false', limit: '1000' } : { after: String(newest), limit: '1000' }, 1000).then(add, () => {})
  })

  const stretch = (start: number, end = Infinity) => {
    if (asked.current.has(start)) return
    asked.current.add(start)
    walk({ after: String(start) }, 200, end).then(
      (got) => {
        add(got.filter((e) => e.id < end))
        setStretches((s) => new Set(s).add(start))
      },
      () => asked.current.delete(start), // asked again at its next opening
    )
  }
  const timeline = <Timeline events={events} read={stretches} onStretch={stretch} />

  if (!task) return <div className="page task-page">{missing && <p>{missing}</p>}</div>

  async function act(action: 'start' | 'cancel') {
    setAsking(false)
    try {
      setTask(await api<TaskView>('POST', `/tasks/${id}/actions`, { action }))
      setRefused('')
    } catch (e) {
      setRefused(e instanceof Error ? e.message : String(e))
      await read()
    }
  }

  const close = () => {
    setEdit(null)
    setRefused('')
  }

  // The fields keep what was typed while reads go on; what is sent is measured against the task when Edit was pressed.
  async function save(e: Editing) {
    const ids = (e.depends.match(/\d+/g) ?? []).map(Number)
    const change: { title?: string; body?: string; depends_on?: number[] } = {}
    if (e.title !== e.was.title) change.title = e.title
    if (e.body !== e.was.body) change.body = e.body
    if (String(ids) !== String(e.was.dependencies.map((d) => d.id))) change.depends_on = ids
    if (Object.keys(change).length === 0) return close()
    setSending(true)
    try {
      setTask(await api<TaskView>('PATCH', `/tasks/${id}`, change))
      close()
    } catch (err) {
      setRefused(err instanceof Error ? err.message : String(err))
    }
    setSending(false)
  }

  const open = task.status !== 'done' && task.status !== 'cancelled'
  const draft = task.status === 'draft'
  const cancelButton = (
    <button className="btn danger" onClick={() => setAsking(true)}>
      Cancel task
    </button>
  )
  const editButton = (
    <button
      className="btn"
      onClick={() => {
        setRefused('')
        setEdit({ was: task, title: task.title, body: task.body, depends: task.dependencies.map((d) => d.id).join(', ') })
      }}
    >
      Edit
    </button>
  )
  const startButton = (
    <button className="btn primary" onClick={() => void act('start')}>
      Start
    </button>
  )
  const saveButton = edit && (
    <button className="btn primary" disabled={sending} onClick={() => void save(edit)}>
      Save
    </button>
  )
  const dropButton = (
    <button className="btn" onClick={close}>
      Cancel
    </button>
  )
  const set = (key: 'title' | 'body' | 'depends', value: string) => setEdit((e) => e && { ...e, [key]: value })
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
      {edit ? (
        <div className="page-head">
          <span className="field title-field">
            <input aria-label="Title" value={edit.title} onChange={(e) => set('title', e.target.value)} />
          </span>
          {!narrow && (
            <>
              {dropButton}
              {saveButton}
            </>
          )}
        </div>
      ) : (
        <PageHead title={task.title}>
          {!narrow && open && (
            <>
              {draft && editButton}
              {draft && startButton}
              {cancelButton}
            </>
          )}
        </PageHead>
      )}
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
      {edit ? (
        <div className="task-edit">
          <div className="field">
            <textarea
              aria-label="Details"
              ref={grow}
              rows={Math.max(narrow ? 8 : 12, edit.body.split('\n').length + 1)}
              value={edit.body}
              onChange={(e) => {
                set('body', e.target.value)
                grow(e.target)
              }}
            />
          </div>
          <Field label="Depends on">
            <input placeholder="Task numbers, such as 42" value={edit.depends} onChange={(e) => set('depends', e.target.value)} />
          </Field>
        </div>
      ) : (
        task.body && <p className="task-body">{task.body}</p>
      )}
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
          {timeline}
          {(edit || open) && (
            <div className="foot-acts">
              {edit ? (
                <>
                  {saveButton}
                  {dropButton}
                </>
              ) : (
                <>
                  {draft && startButton}
                  {draft && editButton}
                  {cancelButton}
                </>
              )}
            </div>
          )}
        </>
      ) : (
        <div className="task-grid">
          <div>
            <h2 className="sec">What happened</h2>
            {timeline}
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
            <button className="btn danger" onClick={() => void act('cancel')}>
              Cancel the task
            </button>
          </div>
        </Dialog>
      )}
    </div>
  )
}
