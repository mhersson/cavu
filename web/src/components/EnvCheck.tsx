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

export default function EnvCheck({ project, onProject }: Props) {
  const [refused, setRefused] = useState('')
  const give = useRef(onProject)
  useEffect(() => {
    give.current = onProject
  })
  const { id, check } = project
  const asked = check.requested_at !== null

  // A check's end records no event, so a check asked for is read until it ends.
  useEffect(() => {
    if (!asked) return
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
  }, [asked, id])

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
  return (
    <section className={`envcheck ${project.runnable ? 'k-ready' : 'k-stop'}`} aria-label="Environment check">
      <p className="l0">
        <i className="mk" />
        <b>{project.runnable ? 'It can run tasks.' : 'It cannot run tasks yet.'}</b>
        <span>
          <Verify p={project} /> <Newest p={project} />
        </span>
      </p>
      {check.output && <pre className="out">{check.output}</pre>}
      {tools && <div className="facts">{tools}</div>}
      {suggested && (
        <div className="facts">
          The repository suggests <code>{check.verify_suggestion}</code> as the verify command.
        </div>
      )}
      {check.requested_at && <div className="facts">A check is asked for since {clock(check.requested_at)}.</div>}
      <div className="acts">
        <button className="btn" onClick={ask}>
          {ended ? 'Check again' : 'Check'}
        </button>
      </div>
      {refused && (
        <p className="refused" role="alert">
          {refused}
        </p>
      )}
    </section>
  )
}
