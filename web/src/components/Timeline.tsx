import { useEffect, useEffectEvent, useState, type ReactNode } from 'react'
import type { PrepareFailed, SessionEnded, TaskEvent, VerifyEnded } from '../api/types.ts'
import { clock, duration } from './time.ts'
import './Timeline.css'

type Props = {
  // In id order.
  events: TaskEvent[]
  // The starts whose stretch of the agent's steps is read.
  read: ReadonlySet<number>
  // Asks for the events after a session's start and before its end; a live session has no end.
  onStretch: (start: number, end?: number) => void
}

type Session = { start: TaskEvent; end?: TaskEvent; steps: TaskEvent[] }
type Item = { run: number; e: TaskEvent } | { row: TaskEvent } | { session: Session }

const account = "The agent's own account"

const time = (iso: string) =>
  new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' })

// The end's day only when it is not the start's.
function until(start: string, end: string): string {
  const e = new Date(end)
  if (e.toDateString() !== new Date(start).toDateString()) return clock(end)
  return e.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
}

// A session runs from its start to the next event that is neither the agent's, a denied request nor a person's.
function arrange(events: TaskEvent[]): Item[] {
  const items: Item[] = []
  let runs = 0
  let live: Session | undefined
  for (const e of events) {
    if (e.actor === 'agent') {
      if (live) live.steps.push(e)
      else items.push({ row: e })
      continue
    }
    if (live && e.actor !== 'user' && e.type !== 'egress_denied') {
      live.end = e
      live = undefined
    }
    items.push(e.type === 'run_started' ? { run: ++runs, e } : { row: e })
    if (e.type === 'session_started' || e.type === 'review_started') {
      live = { start: e, steps: [] }
      items.push({ session: live })
    }
  }
  return items
}

// A person's toggle wins; untouched, a fold is as init says. onOpen is called each time it opens.
function Fold({ init, className, summary, onOpen, children }: { init: boolean; className?: string; summary: ReactNode; onOpen?: () => void; children: ReactNode }) {
  const [chosen, setChosen] = useState<boolean>()
  const open = chosen ?? init
  const opened = useEffectEvent(() => onOpen?.())
  useEffect(() => {
    if (open) opened()
  }, [open])
  return (
    <details className={className} open={open}>
      <summary
        onClick={(e) => {
          e.preventDefault()
          setChosen(!open)
        }}
      >
        {summary}
      </summary>
      {children}
    </details>
  )
}

function Verify({ p }: { p: VerifyEnded }) {
  if (p.result === 'waived') return null
  const how = p.result === 'timeout' ? 'timed out' : `exited ${p.exit_code}`
  return (
    <Fold
      init={p.result === 'failed' || p.result === 'timeout'}
      summary={
        <>
          <code>{p.command}</code> {how} after {duration(p.duration_ms)}
        </>
      }
    >
      <pre className="out">{p.output}</pre>
    </Fold>
  )
}

// Always a failure, and its output is what a person fixes a setup from.
function Prepare({ p }: { p: PrepareFailed }) {
  if (!p.output) return null
  return (
    <Fold init summary="Output">
      <pre className="out">{p.output}</pre>
    </Fold>
  )
}

function Account({ p }: { p: SessionEnded }) {
  if (!p.summary) return null
  const list = (items: string[] | null | undefined, lead: string) =>
    items?.length ? (
      <ul>
        {items.map((s, i) => (
          <li key={i}>
            {lead} {s}
          </li>
        ))}
      </ul>
    ) : null
  return (
    <Fold init={false} summary={account}>
      <div className="said" role="note" aria-label={account}>
        <p>{p.summary}</p>
        {list(p.assumptions, 'It assumed:')}
        {list(p.where_to_look, 'Look at')}
      </div>
    </Fold>
  )
}

function Row({ e }: { e: TaskEvent }) {
  const mark = e.type === 'stopped' ? ' stopped' : e.actor === 'agent' ? ' agent' : e.actor === 'user' ? ' user' : ''
  return (
    <div className={'ev' + mark}>
      <span className="t">{time(e.at)}</span>
      <span className="ty">{e.type}</span>
      <div className="tx">
        {e.text}
        {e.type === 'prepare_failed' && <Prepare p={e.payload as PrepareFailed} />}
        {e.type === 'verify_ended' && <Verify p={e.payload as VerifyEnded} />}
        {e.type === 'session_ended' && <Account p={e.payload as SessionEnded} />}
      </div>
    </div>
  )
}

function Steps({ s, read, onStretch }: { s: Session; read: boolean; onStretch: Props['onStretch'] }) {
  const n = s.steps.length
  const lead = read ? `${n} ${n === 1 ? 'step' : 'steps'} by the agent` : 'Steps by the agent'
  return (
    <Fold
      className="fold"
      init={!s.end}
      summary={`${lead}, ${clock(s.start.at)} to ${s.end ? until(s.start.at, s.end.at) : 'now'}`}
      onOpen={read ? undefined : () => onStretch(s.start.id, s.end?.id)}
    >
      {s.steps.map((e) => (
        <Row key={e.id} e={e} />
      ))}
    </Fold>
  )
}

export default function Timeline({ events, read, onStretch }: Props) {
  return (
    <div className="tl">
      {arrange(events).map((it) =>
        'run' in it ? (
          <h3 key={it.e.id} className="run">
            Run {it.run} <span>{it.e.text}</span>
          </h3>
        ) : 'row' in it ? (
          <Row key={it.row.id} e={it.row} />
        ) : (
          <Steps key={`s${it.session.start.id}`} s={it.session} read={read.has(it.session.start.id)} onStretch={onStretch} />
        ),
      )}
    </div>
  )
}
