import { useEffect, useState, type ReactNode } from 'react'
import { Link } from 'react-router'
import { api } from '../api/client.ts'
import type { Question, Task, TaskEvent } from '../api/types.ts'
import { ChangeFacts, PullRequestFacts, ReviewFacts } from './Facts.tsx'
import { ago, duration } from './time.ts'
import './NeedsBox.css'

// refused is true for the task read again after the server refused an action.
type Props = { task: Task; onTask: (t: Task, refused?: boolean) => void; onPage?: boolean; flat?: boolean }
type Action = { action: 'answer'; text: string } | { action: 'retry' | 'done' }
type Verify = { result: string; command: string; exit_code: number; duration_ms: number; output: string }

// A typed answer by task, so a refusal or the remount after a sign-in keeps it; a reload does not.
const typed = new Map<number, string>()

function Ask({ q }: { q: Question }) {
  return (
    <>
      {q.question}
      {q.suggested_answer && <span className="sug">The agent would assume: {q.suggested_answer}</span>}
    </>
  )
}

function Questions({ qs }: { qs: Question[] }) {
  return qs.length > 1 ? (
    <ol className="qs">
      {qs.map((q, i) => (
        <li key={i}>
          <Ask q={q} />
        </li>
      ))}
    </ol>
  ) : (
    qs.map((q, i) => <Ask key={i} q={q} />)
  )
}

export default function NeedsBox(props: Props) {
  // A pane that shows another task gets a new box: the typed answer, the refusal and the output are one task's.
  return <Box key={props.task.id} {...props} />
}

function Box({ task, onTask, onPage = false, flat = false }: Props) {
  const [text, setText] = useState(() => typed.get(task.id) ?? '')
  const [busy, setBusy] = useState(false)
  const [refused, setRefused] = useState('')
  const [verify, setVerify] = useState<Verify | null>(null)
  const readVerify = flat && task.needs === 'retry' && task.stop?.reason === 'verify_failed'

  // needs_you_since is the stop's time, so a new stop reads its own verify.
  useEffect(() => {
    if (!readVerify) return
    api<{ events: TaskEvent[] }>('GET', `/tasks/${task.id}/events?order=newest&agent=false&limit=10`)
      .then((r) => setVerify((r.events.find((e) => e.type === 'verify_ended')?.payload as Verify | undefined) ?? null))
      .catch(() => {})
  }, [readVerify, task.id, task.needs_you_since])

  // A refusal stays in view with its task, even when the task needs nothing more.
  if (!task.needs && !refused) return null

  async function act(a: Action) {
    setBusy(true)
    try {
      const t = await api<Task>('POST', `/tasks/${task.id}/actions`, a)
      if (a.action === 'answer') {
        typed.delete(task.id)
        setText('')
      }
      setRefused('')
      onTask(t)
    } catch (e) {
      setRefused(e instanceof Error ? e.message : String(e))
      await api<Task>('GET', `/tasks/${task.id}`).then((t) => onTask(t, true), () => {}) // the line shown is the refusal's
    }
    setBusy(false)
  }

  const write = (v: string) => {
    typed.set(task.id, v)
    setText(v)
  }
  const button = (name: string, onClick: () => void, primary = false) => (
    <button className={primary ? 'btn primary' : 'btn'} disabled={busy} onClick={onClick}>
      {name}
    </button>
  )
  const page = `/tasks/${task.id}`
  const open = !onPage && (
    <Link className="btn" to={page}>
      Open task
    </Link>
  )
  const { pull_request: pr, change } = task
  const files = change && (
    <span className="facts">
      {change.files_changed} {change.files_changed === 1 ? 'file' : 'files'} changed.
    </span>
  )
  const since = task.needs_you_since && ago(task.needs_you_since)

  let label = ''
  let lead: ReactNode = null
  let facts: ReactNode = null
  let field: ReactNode = null
  let acts: ReactNode = null
  switch (task.needs) {
    case 'answer': {
      const qs = task.questions
      const several = qs.length > 1
      const assumed = qs
        .flatMap((q, i) => (q.suggested_answer ? [several ? `${i + 1}. ${q.suggested_answer}` : q.suggested_answer] : []))
        .join('\n')
      label = several ? `${qs.length} questions from the agent` : 'A question from the agent'
      lead = <Questions qs={qs} />
      field = (
        <textarea
          aria-label="Answer"
          placeholder={several ? 'Your answer to the questions' : 'Your answer to the question'}
          value={text}
          onChange={(e) => write(e.target.value)}
        />
      )
      acts = (
        <>
          {button('Answer', () => void act({ action: 'answer', text }), true)}
          {assumed && button('Use its assumptions', () => write(assumed))}
        </>
      )
      break
    }
    case 'retry':
      label = 'Stopped: ' + task.stop?.reason.replace(/_/g, ' ')
      lead = task.stop?.explanation
      facts = flat ? (
        <>
          {verify && (
            <>
              <div className="verify">
                <code>{verify.command}</code>{' '}
                {verify.result === 'timeout' ? 'timed out' : `exited ${verify.exit_code}`} after{' '}
                {duration(verify.duration_ms)}
              </div>
              <pre className="out">{verify.output}</pre>
            </>
          )}
          {pr && (
            <div className="rail">
              <PullRequestFacts pr={pr} />
            </div>
          )}
        </>
      ) : (
        pr && (
          <div className="facts">
            Pull request {pr.number}, checks {pr.checks || 'not read yet'}
            {pr.failed_checks.length > 0 && ': ' + pr.failed_checks.join(', ')}
          </div>
        )
      )
      acts = (
        <>
          {button('Retry', () => void act({ action: 'retry' }), true)}
          {open}
        </>
      )
      break
    case 'review': {
      const flagged = change ? [...change.flagged.workflows, ...change.flagged.instructions, ...change.flagged.large] : []
      label = 'Ready for your review'
      lead = flat
        ? task.waiting
        : pr && (
            <>
              Pull request {pr.number}, checks {pr.checks}. {files}
            </>
          )
      facts = flat ? (
        <div className="rail">
          <PullRequestFacts pr={pr} />
          <ChangeFacts change={change} />
          <ReviewFacts review={task.review} />
        </div>
      ) : (
        flagged.length > 0 &&
        flagged.map((p) => (
          <span key={p} className="flag">
            Touches {p}
          </span>
        ))
      )
      acts = (
        <>
          <a className="btn primary" href={pr?.url} target="_blank" rel="noreferrer">
            Open pull request
          </a>
          {flat && open}
        </>
      )
      break
    }
    case 'take':
      label = 'A change to take by hand'
      lead = !flat && files
      facts = flat && (
        <div className="rail">
          <ChangeFacts change={change} />
        </div>
      )
      acts = (
        <>
          <a className="btn" href={`/api/tasks/${task.id}/change?format=bundle`} download>
            Download the change
          </a>
          <a className="btn link" href={`/api/tasks/${task.id}/change?format=diff`} download>
            Diff
          </a>
          {button('Record the merge', () => void act({ action: 'done' }), true)}
        </>
      )
      break
  }

  const alert = refused && (
    <p role="alert" className="refused">
      {refused}
    </p>
  )

  if (flat) {
    return (
      <div className={`needs flat k-${task.needs}`}>
        {label && (
          <div className="kind">
            <i className="mk" />
            {label}
          </div>
        )}
        <h2>
          <Link to={page}>{task.title}</Link>
        </h2>
        <div className="meta">
          Task {task.id} in {task.project}
          {since && `, ${since}`}
        </div>
        {lead && <div className="lead">{lead}</div>}
        {field}
        {facts}
        <div className="acts">{acts}</div>
        {alert}
      </div>
    )
  }

  return (
    <article className={`needs strip k-${task.needs}`}>
      {label && (
        <div className="l0">
          <i className="mk" />
          <span className="kind">{label}</span>
          <span className="age">{since}</span>
        </div>
      )}
      {!onPage && (
        <div className="l1">
          <span className="mono">{task.id}</span>
          <span className="mono">{task.project}</span>
          <Link className="title" to={page}>
            {task.title}
          </Link>
        </div>
      )}
      <div className="l2">
        <div className="why">{lead}</div>
        {!field && <div className="acts">{acts}</div>}
      </div>
      {facts && <div className="more">{facts}</div>}
      {field && (
        <div className="answer">
          {field}
          <div className="acts">{acts}</div>
        </div>
      )}
      {alert}
    </article>
  )
}
