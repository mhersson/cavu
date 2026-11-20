import { useCallback, useEffect, useRef, useState, type ChangeEvent, type FormEvent, type ReactNode } from 'react'
import { Link } from 'react-router'
import { api } from '../../api/client.ts'
import type { Project, Settings as Saved } from '../../api/types.ts'
import EnvCheck from '../../components/EnvCheck.tsx'
import Field from '../../components/Field.tsx'
import { NewTaskDialog } from '../../components/NewTaskDialog.tsx'
import PageHead from '../../components/PageHead.tsx'
import { useVersion } from '../../components/useVersion.ts'
import { NewProjectForm } from '../Projects/NewProject.tsx'
import './Setup.css'

type Body = { settings?: Record<string, string | null>; secrets?: Record<string, string> }
type StepProps = { saved: Saved; onSaved: (s?: Saved) => void }

const line = (e: unknown) => (e instanceof Error ? e.message : String(e))

const text = (s: Saved, k: string) => {
  const v = s.settings[k]
  return typeof v === 'string' ? v : ''
}

const pick = (s: Saved, keys: string[]) => Object.fromEntries(keys.map((k) => [k, text(s, k)]))

const modes: [string, string][] = [
  ['subscription', 'A subscription'],
  ['key', 'A key and an endpoint'],
]

// A set secret's field is empty and says so; left empty, it is not sent.
const kept = (set: boolean) => (set ? 'Set. Left empty, it is kept.' : undefined)

function endpointSet(s: Saved) {
  const mode = text(s, 'endpoint_mode')
  const reached = mode !== 'key' || (text(s, 'endpoint_url') !== '' && text(s, 'endpoint_auth_header') !== '')
  return mode !== '' && text(s, 'model_work') !== '' && s.secrets.endpoint_credential && reached
}

// The default credential as the server reads it, an App id before a token, else the named ones; "" for none.
function credential(s: Saved) {
  const app = text(s, 'forge_app_id')
  if (app ? s.secrets.forge_app_key : s.secrets.forge_token) return app ? `A GitHub App, id ${app}` : 'A token'
  return s.forge_credentials.length > 0 ? `The credentials ${s.forge_credentials.map((c) => c.name).join(', ')}` : ''
}

const githubSet = (s: Saved) => credential(s) !== '' && text(s, 'commit_name') !== '' && text(s, 'commit_email') !== ''

// What a Save sends: each setting that differs from the saved one, then the parts given; an empty part is left out.
function body(s: Saved, typed: Record<string, string>, more: Body = {}): Body {
  const settings = { ...Object.fromEntries(Object.entries(typed).filter(([k, v]) => v !== text(s, k))), ...more.settings }
  const b: Body = {}
  if (Object.keys(settings).length > 0) b.settings = settings
  if (more.secrets) b.secrets = more.secrets
  return b
}

// Save sends what build gives, and nothing when that is empty; a refusal keeps what was typed.
function StepForm({ build, onSaved, children }: { build: () => Promise<Body>; onSaved: (s?: Saved) => void; children: ReactNode }) {
  const [refused, setRefused] = useState('')
  const [sending, setSending] = useState(false)

  async function save(e: FormEvent) {
    e.preventDefault()
    setRefused('')
    setSending(true)
    try {
      const b = await build()
      onSaved(b.settings || b.secrets ? await api<Saved>('PATCH', '/settings', b) : undefined)
    } catch (err) {
      setRefused(line(err))
      setSending(false)
    }
  }

  return (
    <form onSubmit={(e) => void save(e)}>
      {children}
      {refused && (
        <p className="refused" role="alert">
          {refused}
        </p>
      )}
      <button className="btn primary" disabled={sending}>
        Save and go on
      </button>
    </form>
  )
}

function Endpoint({ saved, onSaved }: StepProps) {
  const [f, setF] = useState(() => pick(saved, ['endpoint_mode', 'endpoint_url', 'endpoint_auth_header', 'model_work']))
  const [secret, setSecret] = useState('')
  const set = (k: string) => (e: ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value })
  const key = f.endpoint_mode === 'key'

  // The address and the header are the key mode's alone.
  const build = async () =>
    body(saved, key ? f : { endpoint_mode: f.endpoint_mode, model_work: f.model_work }, secret ? { secrets: { endpoint_credential: secret } } : {})

  return (
    <StepForm build={build} onSaved={onSaved}>
      <div className="field">
        <div className="seg" role="radiogroup" aria-label="Mode">
          {modes.map(([v, label]) => (
            <button key={v} type="button" role="radio" aria-checked={v === f.endpoint_mode} className={v === f.endpoint_mode ? 'on' : ''} onClick={() => setF({ ...f, endpoint_mode: v })}>
              {label}
            </button>
          ))}
        </div>
      </div>
      {key && (
        <>
          <Field label="Endpoint address">
            <input className="mono" spellCheck={false} value={f.endpoint_url} onChange={set('endpoint_url')} />
          </Field>
          <Field label="Auth header">
            <input className="mono" spellCheck={false} value={f.endpoint_auth_header} onChange={set('endpoint_auth_header')} />
          </Field>
        </>
      )}
      {f.endpoint_mode !== '' && (
        <Field label={key ? 'The key' : "The subscription's token"}>
          <input type="password" autoComplete="new-password" placeholder={kept(saved.secrets.endpoint_credential)} value={secret} onChange={(e) => setSecret(e.target.value)} />
        </Field>
      )}
      <Field label="Work model">
        <input value={f.model_work} onChange={set('model_work')} />
      </Field>
      <p className="small">CAVU seals a secret and never shows it again.</p>
      <p className="small">
        <code>cavu doctor</code> on the server tests the endpoint.
      </p>
    </StepForm>
  )
}

function GitHub({ saved, onSaved }: StepProps) {
  const [f, setF] = useState(() => pick(saved, ['forge_app_id', 'commit_name', 'commit_email']))
  const [app, setApp] = useState(f.forge_app_id !== '')
  const [token, setToken] = useState('')
  const file = useRef<HTMLInputElement>(null)
  const set = (k: string) => (e: ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value })

  // The default is written as Settings writes it, clearing the other kind; the key is read only as it is sent.
  async function build() {
    const commits = { commit_name: f.commit_name, commit_email: f.commit_email }
    if (app) {
      const key = await file.current?.files?.[0]?.text()
      return body(saved, { ...commits, forge_app_id: f.forge_app_id }, key === undefined ? {} : { secrets: { forge_app_key: key, forge_token: '' } })
    }
    return body(saved, commits, token ? { settings: { forge_app_id: null }, secrets: { forge_token: token, forge_app_key: '' } } : {})
  }

  return (
    <StepForm build={build} onSaved={onSaved}>
      {app ? (
        <>
          <Field label="App id">
            <input className="mono" value={f.forge_app_id} onChange={set('forge_app_id')} />
          </Field>
          <Field label="Private key">
            <input type="file" accept=".pem" ref={file} />
          </Field>
          {saved.secrets.forge_app_key && <p className="small">The key is set. With no file chosen it is kept.</p>}
        </>
      ) : (
        <Field label="A token for the repositories CAVU works on">
          <input type="password" autoComplete="new-password" placeholder={kept(saved.secrets.forge_token)} value={token} onChange={(e) => setToken(e.target.value)} />
        </Field>
      )}
      <p className="small">
        CAVU pushes branches and opens draft pull requests with it. It never merges.{' '}
        <button type="button" className="btn link" onClick={() => setApp(!app)}>
          {app ? 'Use a token instead' : 'Use a GitHub App instead'}
        </button>
      </p>
      <Field label="Name on commits">
        <input value={f.commit_name} onChange={set('commit_name')} />
      </Field>
      <Field label="Email on commits">
        <input value={f.commit_email} onChange={set('commit_email')} />
      </Field>
    </StepForm>
  )
}

export default function Setup() {
  const version = useVersion()
  const [saved, setSaved] = useState<Saved | null>(null)
  const [projects, setProjects] = useState<Project[] | null>(null)
  const [failed, setFailed] = useState('')
  const [changing, setChanging] = useState(0)
  const [saves, setSaves] = useState(0)
  const [made, setMade] = useState('')
  const [tasking, setTasking] = useState(false)

  const read = useCallback(() => {
    api<{ projects: Project[] }>('GET', '/projects').then((r) => setProjects(r.projects), (e: unknown) => setFailed(line(e)))
  }, [])
  useEffect(() => {
    api<Saved>('GET', '/settings').then(setSaved, (e: unknown) => setFailed(line(e)))
    read()
  }, [read])

  // A save remounts the open form, so a secret it sent is held no longer.
  function onSaved(s?: Saved) {
    if (s) setSaved(s)
    setChanging(0)
    setSaves((n) => n + 1)
  }

  function created(name: string) {
    setMade(name)
    read()
  }

  const put = (p: Project) => setProjects((l) => l && l.map((x) => (x.id === p.id ? p : x)))

  function steps(s: Saved, list: Project[]) {
    const done = [endpointSet(s), githubSet(s), list.length > 0]
    // The step Change chose, else the first not done, else the check.
    const open = changing || done.indexOf(false) + 1 || 4
    const project = list.find((p) => p.name === made) ?? list.find((p) => p.runnable) ?? list[0]
    const change = (n: number) => (
      <button className="btn link" onClick={() => setChanging(n)}>
        Change
      </button>
    )
    const parts: { title: string; sum?: ReactNode; form: ReactNode }[] = [
      {
        title: 'The model endpoint',
        sum: (
          <>
            {text(s, 'endpoint_mode') === 'key' ? `A key and the endpoint ${text(s, 'endpoint_url')}` : 'A subscription'}, with the work model {text(s, 'model_work')}. {change(1)}
          </>
        ),
        form: <Endpoint key={saves} saved={s} onSaved={onSaved} />,
      },
      {
        title: 'GitHub',
        sum: (
          <>
            {credential(s)}. Commits by {text(s, 'commit_name')} &lt;{text(s, 'commit_email')}&gt;. {change(2)}
          </>
        ),
        form: <GitHub key={saves} saved={s} onSaved={onSaved} />,
      },
      {
        title: 'The first project',
        sum: project && (
          <>
            {project.name}, {project.repo_url} <Link to={`/projects/${project.name}`}>Change</Link>
          </>
        ),
        form: <NewProjectForm submit="Create and check" onCreated={created} />,
      },
      {
        title: 'The environment check',
        form: project && (
          <div className="now">
            <EnvCheck project={project} onProject={put} />
            {project.check.requested_at === null &&
              (project.runnable ? (
                <button className="btn primary" onClick={() => setTasking(true)}>
                  Create the first task
                </button>
              ) : (
                <Link className="btn" to={`/projects/${project.name}`}>
                  Open the project
                </Link>
              ))}
          </div>
        ),
      },
    ]
    return parts.map((p, i) => {
      const n = i + 1
      const state = n === open ? 'open' : done[i] ? 'done' : 'later'
      return (
        <section key={n} className={`step ${state}`} aria-labelledby={`setup-${n}`} aria-current={n === open ? 'step' : undefined}>
          <div className="n">{state === 'done' ? '✓' : n}</div>
          <div>
            <h2 id={`setup-${n}`}>{p.title}</h2>
            {state === 'open' && p.form}
            {state === 'done' && <p className="sum">{p.sum}</p>}
          </div>
        </section>
      )
    })
  }

  return (
    <>
      <div className="brand-bar">
        <span className="brand">CAVU</span>
        <span>{version}</span>
      </div>
      <main className="main">
        <div className="page">
          <div className="setup">
            <PageHead title="Set up CAVU" />
            <p className="sub">Four steps. After the last one the first task can start.</p>
            {failed && (
              <p className="refused" role="alert">
                {failed}
              </p>
            )}
            {saved && projects && steps(saved, projects)}
          </div>
        </div>
      </main>
      {tasking && <NewTaskDialog onClose={() => setTasking(false)} />}
    </>
  )
}
