import { useEffect, useRef, useState } from 'react'
import { api } from '../api/client.ts'
import type { Project } from '../api/types.ts'
import { clock } from './time.ts'
import './EnvCheck.css'

type Props = { project: Project; onProject: (p: Project) => void }

// The verify fact: the first that holds.
function Verify({ p }: { p: Project }) {
  const b = p.baseline
  if (p.verify_waived) return <>A person checks every change.</>
  if (b)
    return (
      <>
        The verify command {b.passed ? 'passed' : 'has not passed'}{b.commit && (
          <>
            {' '}on base commit <code>{b.commit}</code>
          </>
        )}{' '}at {clock(b.at)}.
      </>
    )
  if (!p.verify_command) return <>There is no verify command.</>
  return <>No result of the verify command is stored.</>
}

function Newest({ p }: { p: Project }) {
  const c = p.check
  if (c.fault)
    return (
      <>
        The environment check met an error: <span className="fault">{c.fault}</span>
      </>
    )
  if (c.failed_step)
    return (
      <>
        The environment check failed at the step <code>{c.failed_step}</code>
        {c.commit && (
          <>
            , on commit <code>{c.commit}</code>
          </>
        )}
        .
      </>
    )
  if (c.commit) {
    if (p.baseline?.passed) return null
    return (
      <>
        The environment check passed its steps on commit <code>{c.commit}</code>.
      </>
    )
  }
  return <>No environment check has ended yet.</>
}

function Underway({ running, since }: { running: boolean; since: string }) {
  if (running)
    return (
      <>
        <b>The environment check is running.</b>
        <span>It started at {clock(since)}.</span>
      </>
    )
  return (
    <>
      <b>The environment check waits to start.</b>
      <span>It was asked for at {clock(since)}. It starts when a slot is free and no other project's check runs.</span>
    </>
  )
}

export default function EnvCheck({ project, onProject }: Props) {
  const [refused, setRefused] = useState('')
  const give = useRef(onProject)
  useEffect(() => {
    give.current = onProject
  })
  const { id, check } = project
  // The server clears started_at only after it stored the result, which clears requested_at.
  const since = check.started_at ?? check.requested_at
  const underway = since !== null

  // A check's end records no event, so a check under way is read until it ends.
  useEffect(() => {
    if (!underway) return
    let live = true
    const timer = setInterval(() => {
      api<Project>('GET', `/projects/${id}`).then(
        (p) => live && give.current(p),
        () => {},
      )
    }, 3000)
    return () => {
      live = false
      clearInterval(timer)
    }
  }, [underway, id])

  async function ask() {
    setRefused('')
    try {
      onProject(await api<Project>('POST', `/projects/${id}/check`))
    } catch (e) {
      setRefused(e instanceof Error ? e.message : String(e))
    }
  }

  const ended = check.fault !== '' || check.failed_step !== '' || check.commit !== ''
  const tools = [
    check.tools.length > 0 && `Tools found: ${check.tools.join(', ')}.`,
    check.ignored_tools.length > 0 && `Left out: ${check.ignored_tools.join(', ')}.`,
  ]
    .filter(Boolean)
    .join(' ')
  const suggested = check.verify_suggestion !== '' && check.verify_suggestion !== project.verify_command
  const first = project.baseline === null && !ended
  const kind = check.started_at ? 'k-run' : underway ? 'k-wait' : project.runnable ? 'k-ready' : 'k-stop'
  const facts = (
    <>
      <Verify p={project} /> <Newest p={project} />
    </>
  )
  return (
    <section className={`envcheck ${kind}`} aria-label="Environment check">
      <p className="l0">
        <i className="mk" />
        {underway ? (
          <Underway running={check.started_at !== null} since={since} />
        ) : (
          <>
            <b>{project.runnable ? 'It can run tasks.' : 'It cannot run tasks yet.'}</b>
            <span>{facts}</span>
          </>
        )}
      </p>
      {underway &&
        (first ? (
          <p className="later first">
            CAVU asked for this first check by itself, so there is nothing to do but wait. A first check downloads the project's
            tools and can take many minutes.
          </p>
        ) : (
          <p className="later">{facts}</p>
        ))}
      {check.output && <pre className="out">{check.output}</pre>}
      {tools && <div className="facts">{tools}</div>}
      {suggested && (
        <div className="facts">
          The repository suggests <code>{check.verify_suggestion}</code> as the verify command.
        </div>
      )}
      {!underway && (
        <div className="acts">
          <button className="btn" onClick={ask}>
            {ended ? 'Check again' : 'Check'}
          </button>
        </div>
      )}
      {refused && (
        <p className="refused" role="alert">
          {refused}
        </p>
      )}
    </section>
  )
}
