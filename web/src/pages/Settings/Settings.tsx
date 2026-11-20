import { useEffect, useState, type ReactNode } from 'react'
import { api } from '../../api/client.ts'
import type { Settings as Saved } from '../../api/types.ts'
import PageHead from '../../components/PageHead.tsx'
import { useNarrow } from '../../components/useNarrow.ts'
import { useLook } from '../../theme/theme.ts'
import type { Look } from '../../theme/theme.ts'
import Choice from './Choice.tsx'
import Credentials from './Credentials.tsx'
import './Settings.css'

const palettes: [Look['palette'], string][] = [
  ['copper', 'Copper'],
  ['catppuccin', 'Catppuccin'],
]

const themes: [Look['theme'], string][] = [
  ['system', 'As the system'],
  ['dark', 'Dark'],
  ['light', 'Light'],
]

// keys is one key, or a model and its effort; a secret row's keys are secrets.
// A keyOnly row is dimmed unless the mode is a key and an endpoint.
type Row = { label: string; keys: string[]; mono?: boolean; keyOnly?: boolean; options?: [string, string][]; secret?: boolean }

// head draws "Model" and "Effort" over the pairs; rest takes every key no section places;
// credentials is the GitHub table, which has no Save.
type Section = { id: string; title: string; rows: Row[]; head?: boolean; note?: ReactNode; rest?: boolean; credentials?: boolean }

type Patch = { settings?: Record<string, string | null>; secrets?: Record<string, string> }

const layout: Section[] = [
  {
    id: 'endpoint',
    title: 'Model endpoint',
    rows: [
      { label: 'Mode', keys: ['endpoint_mode'], options: [['subscription', 'A subscription'], ['key', 'A key and an endpoint']] },
      { label: 'Endpoint address', keys: ['endpoint_url'], mono: true, keyOnly: true },
      { label: 'Auth header', keys: ['endpoint_auth_header'], mono: true, keyOnly: true },
      { label: 'Credential', keys: ['endpoint_credential'], secret: true },
    ],
  },
  {
    id: 'models',
    title: 'Models',
    head: true,
    rows: [
      { label: 'Work', keys: ['model_work', 'effort_work'] },
      { label: 'Review', keys: ['model_review', 'effort_review'] },
      { label: 'Late fix rounds', keys: ['model_strong', 'effort_strong'] },
    ],
  },
  {
    id: 'limits',
    title: 'Limits',
    rows: [
      { label: 'A task, minutes', keys: ['task_timeout_minutes'] },
      { label: 'A task, turns', keys: ['task_max_turns'] },
      { label: 'Verify, minutes', keys: ['verify_timeout_minutes'] },
      { label: 'Checks on a pull request, minutes', keys: ['ci_timeout_minutes'] },
      { label: 'Tasks at once', keys: ['concurrency'] },
      { label: 'A task, dollars', keys: ['task_budget_usd'], keyOnly: true },
      { label: 'A day, dollars', keys: ['daily_budget_usd'], keyOnly: true },
      { label: 'Model prices', keys: ['model_prices'], mono: true, keyOnly: true },
    ],
    note: 'The three limits in dollars hold only with a key and an endpoint.',
  },
  { id: 'github', title: 'GitHub', rows: [], credentials: true },
  {
    id: 'commits',
    title: 'Commits',
    rows: [
      { label: 'Name', keys: ['commit_name'] },
      { label: 'Email', keys: ['commit_email'] },
    ],
  },
  {
    id: 'server',
    title: 'The server',
    rest: true,
    rows: [
      { label: 'Container runtime', keys: ['runtime'], mono: true },
      { label: 'CA bundle', keys: ['ca_bundle_path'], mono: true },
      { label: 'Listens on', keys: ['listen_address'], mono: true },
      { label: 'Skills directory', keys: ['skills_path'], mono: true },
      { label: 'Instructions file', keys: ['instructions_path'], mono: true },
    ],
    note: (
      <>
        API tokens and the password are made on the server's command line: <code>cavu token add NAME</code> and <code>cavu user set NAME &lt; FILE</code>.
      </>
    ),
  },
]

// The hint of a key whose default is "", by why it has none.
const hints: Record<string, string> = {
  model_work: 'required',
  commit_name: 'required',
  commit_email: 'required',
  endpoint_url: 'required',
  endpoint_auth_header: 'required',
  model_review: 'as Work',
  effort_review: 'as Work',
  model_strong: 'as Work',
  effort_strong: 'as Work',
  effort_work: "the agent's default",
  runtime: 'docker, else podman',
  task_budget_usd: 'no cap',
  daily_budget_usd: 'no cap',
  model_prices: 'none',
  ca_bundle_path: 'none',
  skills_path: 'none',
  instructions_path: 'none',
}

// forge_app_id is the GitHub section's.
const placed = new Set(['forge_app_id', ...layout.flatMap((s) => s.rows.flatMap((r) => r.keys))])

function sections(defaults: Record<string, string>): Section[] {
  const rest = Object.keys(defaults)
    .filter((k) => !placed.has(k))
    .sort()
    .map((k) => ({ label: k, keys: [k], mono: true }))
  return layout.map((s) => (s.rest ? { ...s, rows: [...s.rows, ...rest] } : s))
}

const line = (e: unknown) => (e instanceof Error ? e.message : String(e))

export default function Settings() {
  const [look, setLook] = useLook()
  const narrow = useNarrow()
  const [saved, setSaved] = useState<Saved | null>(null)
  const [failed, setFailed] = useState('')
  const [typed, setTyped] = useState<Record<string, string>>({})
  const [refused, setRefused] = useState<Record<string, string>>({})
  const [sending, setSending] = useState<Record<string, boolean>>({})

  useEffect(() => {
    api<Saved>('GET', '/settings').then(setSaved, (e: unknown) => setFailed(line(e)))
  }, [])

  const type = (k: string, v: string) => setTyped((t) => ({ ...t, [k]: v }))
  const was = (k: string) => {
    const v = saved?.settings[k]
    return v === undefined ? '' : typeof v === 'string' ? v : JSON.stringify(v)
  }
  const mode = typed.endpoint_mode ?? was('endpoint_mode')
  const off = (r: Row) => !!r.keyOnly && mode !== 'key'

  // A dimmed row sends nothing, so going back to a subscription sends the mode alone.
  function patch(s: Section): Patch {
    const settings: Record<string, string | null> = {}
    const secrets: Record<string, string> = {}
    for (const r of s.rows) {
      if (off(r)) continue
      for (const k of r.keys) {
        const t = typed[k]
        if (r.secret) {
          if (t) secrets[k] = t
        } else if (t !== undefined && t !== was(k)) settings[k] = t || null
      }
    }
    const p: Patch = {}
    if (Object.keys(settings).length > 0) p.settings = settings
    if (Object.keys(secrets).length > 0) p.secrets = secrets
    return p
  }

  async function save(s: Section, p: Patch) {
    setSending((b) => ({ ...b, [s.id]: true }))
    setRefused((r) => ({ ...r, [s.id]: '' }))
    try {
      setSaved(await api<Saved>('PATCH', '/settings', p))
      const mine = new Set(s.rows.flatMap((r) => r.keys))
      setTyped((t) => Object.fromEntries(Object.entries(t).filter(([k]) => !mine.has(k))))
    } catch (e) {
      setRefused((r) => ({ ...r, [s.id]: line(e) }))
    }
    setSending((b) => ({ ...b, [s.id]: false }))
  }

  // The input stays open while typed holds the secret's name, even as "".
  function secret(d: Saved, k: string, id: string) {
    return k in typed ? (
      <input id={id} type="password" className="in" autoComplete="new-password" autoFocus value={typed[k]} onChange={(e) => type(k, e.target.value)} />
    ) : (
      <div className="secret">
        <span>{d.secrets[k] ? 'Set. Its value is never shown.' : 'Not set.'}</span>
        <button className="btn" onClick={() => type(k, '')}>
          {d.secrets[k] ? 'Replace' : 'Set'}
        </button>
      </div>
    )
  }

  function fields(d: Saved, r: Row, keys: string[], id: string, dim: boolean) {
    const inputs = keys.map((k, i) => (
      <input
        key={k}
        id={i ? undefined : id}
        aria-label={i ? r.label + ' effort' : undefined}
        type="text"
        className={r.mono ? 'in mono' : 'in'}
        autoComplete="off"
        spellCheck={r.mono ? false : undefined}
        disabled={dim}
        placeholder={dim ? undefined : d.defaults[k] || hints[k]}
        value={(dim ? undefined : typed[k]) ?? was(k)}
        onChange={(e) => type(k, e.target.value)}
      />
    ))
    return inputs.length > 1 ? <div className="pair">{inputs}</div> : inputs
  }

  function row(d: Saved, r: Row) {
    const keys = r.keys.filter((k) => k in (r.secret ? d.secrets : d.defaults))
    if (keys.length === 0) return null
    const id = 'set-' + keys[0]
    if (r.options) {
      return <Choice key={id} id={id} label={r.label} options={r.options} value={typed[keys[0]] ?? was(keys[0])} onChange={(v) => type(keys[0], v)} />
    }
    const dim = off(r)
    return (
      <div key={id} className={dim ? 'frow off' : 'frow'}>
        <label htmlFor={id}>{r.label}</label>
        {r.secret ? secret(d, keys[0], id) : fields(d, r, keys, id, dim)}
      </div>
    )
  }

  function body(d: Saved, s: Section) {
    const p = patch(s)
    return (
      <>
        {s.head && (
          <div className="frow">
            <span />
            <div className="pair small">
              <span>Model</span>
              <span>Effort</span>
            </div>
          </div>
        )}
        {s.rows.map((r) => row(d, r))}
        {s.note && <p className="note">{s.note}</p>}
        {refused[s.id] && (
          <p role="alert" className="note err">
            {refused[s.id]}
          </p>
        )}
        <button className="btn save" disabled={sending[s.id] || Object.keys(p).length === 0} onClick={() => void save(s, p)}>
          Save
        </button>
      </>
    )
  }

  const parts = [
    ...(saved
      ? sections(saved.defaults).map((s) => ({
          id: s.id,
          title: s.title,
          body: s.credentials ? <Credentials saved={saved} onSaved={setSaved} /> : body(saved, s),
        }))
      : []),
    {
      id: 'browser',
      title: 'This browser',
      body: (
        <>
          <Choice id="look-palette" label="Palette" options={palettes} value={look.palette} onChange={(palette) => setLook({ ...look, palette })} />
          <Choice id="look-theme" label="Theme" options={themes} value={look.theme} onChange={(theme) => setLook({ ...look, theme })} />
          <p className="sub">Applied at once. It holds for this browser only.</p>
        </>
      ),
    },
  ]

  return (
    <div className="page set-page">
      <div className="col">
        <PageHead title="Settings" />
        <p className="sub">They hold for every project. A project can set its own limits.</p>
        {failed && (
          <p role="alert" className="note err">
            {failed}
          </p>
        )}
        {narrow ? (
          <div className="set">
            {parts.map((p, i) => (
              <details key={p.id} className="acc" open={i === 0}>
                <summary>{p.title}</summary>
                <section>{p.body}</section>
              </details>
            ))}
          </div>
        ) : (
          <div className="set-grid">
            <nav className="set-nav">
              {parts.map((p) => (
                <a key={p.id} href={'#' + p.id}>
                  {p.title}
                </a>
              ))}
            </nav>
            <div className="set">
              {parts.map((p) => (
                <section key={p.id} id={p.id}>
                  <h2>{p.title}</h2>
                  {p.body}
                </section>
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
