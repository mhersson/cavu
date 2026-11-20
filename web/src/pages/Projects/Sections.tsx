import { Fragment, type ReactNode } from 'react'
import type { Project } from '../../api/types.ts'
import { pairs, type Edit, type Form } from './edit.ts'

type Props = { project: Project; folded: boolean; onRemove: () => void; edit: Edit | null }
type Row = [ReactNode, ReactNode]
// The field a row's value becomes while editing, named as the row is.
type Field = (label: string, e: Edit) => ReactNode
type Text = { [K in keyof Form]: Form[K] extends string ? K : never }[keyof Form]

const small = (text: string) => <span className="small">{text}</span>

// A value in the mono face, or "none" when it is empty.
const mono = (value: string) => (value ? <code>{value}</code> : small('none'))

function list(values: string[], hint = 'none') {
  if (values.length === 0) return small(hint)
  return values.map((v, i) => (
    <Fragment key={v}>
      {i > 0 && ', '}
      <code>{v}</code>
    </Fragment>
  ))
}

const plain = (values: string[]) => values.join(', ') || small('none')

function kv(rows: Row[]) {
  return (
    <dl className="kv">
      {rows.map(([k, v], i) => (
        <Fragment key={i}>
          <dt>{k}</dt>
          <dd>{v}</dd>
        </Fragment>
      ))}
    </dl>
  )
}

const input =
  (key: Text, face = 'in mono'): Field =>
  (label, e) => <input className={face} aria-label={label} value={e.form[key]} onChange={(v) => e.set(key, v.target.value)} />

// A row per entry, at least one, so an empty or one-line list looks like an input.
const lines =
  (key: Text, face = 'in mono'): Field =>
  (label, e) => (
    <textarea
      className={face}
      aria-label={label}
      rows={Math.max(1, e.form[key].split('\n').length)}
      value={e.form[key]}
      onChange={(v) => e.set(key, v.target.value)}
    />
  )

const cap =
  (key: string): Field =>
  (label, e) => (
    <input
      className="in"
      aria-label={label}
      placeholder="the instance's setting"
      value={e.form.caps[key] ?? ''}
      onChange={(v) => e.set('caps', { ...e.form.caps, [key]: v.target.value })}
    />
  )

// The tick sets its one field: the server, not the page, refuses a waiver beside a verify command.
const tick =
  (key: 'verify_waived' | 'no_ci', text: string): Field =>
  (_, e) => (
    <label className="inline">
      <input type="checkbox" checked={e.form[key]} onChange={(v) => e.set(key, v.target.checked)} />
      {text}
    </label>
  )

const credential: Field = (label, e) => (
  <select className="in" aria-label={label} value={e.form.forge_credential} onChange={(v) => e.set('forge_credential', v.target.value)}>
    <option value="">the default</option>
    {e.credentials.map((c) => (
      <option key={c.name} value={c.name}>
        {c.name}
      </option>
    ))}
  </select>
)

// A secret is written and removed by name; its value is never read back, since the project holds none.
function Secrets({ names, e }: { names: string[]; e: Edit }) {
  const { removed, added } = e.form
  const put = (i: number, s: Partial<Form['added'][number]>) => e.set('added', added.map((x, j) => (j === i ? { ...x, ...s } : x)))
  return (
    <div className="secrets">
      {names.map((n) => {
        const gone = removed.includes(n)
        return (
          <div className="secret" key={n}>
            <code className={gone ? 'gone' : undefined}>{n}</code>
            <button className="btn" onClick={() => e.set('removed', gone ? removed.filter((x) => x !== n) : [...removed, n])}>
              {gone ? 'Keep' : 'Remove'}
            </button>
          </div>
        )
      })}
      {added.map((s, i) => (
        <div className="pair" key={i}>
          <input className="in mono" aria-label="Secret name" placeholder="NAME" value={s.name} onChange={(v) => put(i, { name: v.target.value })} />
          <input
            className="in mono"
            type="password"
            aria-label="Secret value"
            autoComplete="new-password"
            value={s.value}
            onChange={(v) => put(i, { value: v.target.value })}
          />
        </div>
      ))}
      <button className="btn" onClick={() => e.set('added', [...added, { name: '', value: '' }])}>
        Add a secret
      </button>
    </div>
  )
}

// A desktop lays the sections in two columns; a phone folds each.
export default function Sections({ project: p, folded, onRemove, edit: e }: Props) {
  const row = (label: string, read: ReactNode, field?: Field): Row => [label, e && field ? field(label, e) : read]
  const limit = (label: string, key: string) => row(label, p.caps[key] || small("the instance's setting"), cap(key))
  const checked = p.check.fault !== '' || p.check.failed_step !== '' || p.check.commit !== ''
  const sections: [string, ReactNode][] = [
    ['Repository', kv([row('Address', mono(p.repo_url), input('repo_url')), row('Base branch', mono(p.base_branch), input('base_branch'))])],
    [
      'Commands',
      kv([
        row('Setup', mono(p.setup_command), input('setup_command')),
        row('Verify', p.verify_waived ? 'Waived: a person checks every change.' : mono(p.verify_command), input('verify_command')),
        ...(e ? [row('', null, tick('verify_waived', 'No verify command: a person checks every change'))] : []),
      ]),
    ],
    [
      'Tools',
      kv([
        row('Found by the check', checked ? plain(p.check.tools) : small('not checked yet')),
        row('Versions set here', list(pairs(p.tool_overrides)), lines('tool_overrides')),
      ]),
    ],
    [
      'The sandbox',
      kv([
        row('Hosts it may reach', list(p.egress_hosts, 'the package registries of its languages'), lines('egress_hosts')),
        row('Environment', list(pairs(p.env)), lines('env')),
        [
          'Secrets',
          e ? (
            <Secrets names={p.secret_vars} e={e} />
          ) : p.secret_vars.length > 0 ? (
            <>
              {list(p.secret_vars)} {small('set; a value is never shown')}
            </>
          ) : (
            small('none')
          ),
        ],
      ]),
    ],
    ["Limits for this project's tasks", kv([limit('Minutes', 'task_timeout_minutes'), limit('Turns', 'task_max_turns'), limit('Dollars', 'task_budget_usd')])],
    [
      'Steps',
      kv(
        Object.entries(e ? e.form.switches : p.switches).map(([name, on]) => [
          <code>{name}</code>,
          e ? (
            <label className="inline">
              <input type="checkbox" aria-label={name} checked={on} onChange={(v) => e.set('switches', { ...e.form.switches, [name]: v.target.checked })} />
              on
            </label>
          ) : on ? (
            'on'
          ) : (
            small('off')
          ),
        ]),
      ),
    ],
    [
      'GitHub',
      kv([
        row('Credential', p.forge_credential ? <code>{p.forge_credential}</code> : 'the default', credential),
        row('Checks', p.no_ci ? 'This repository has no CI.' : 'Watched after every push.', tick('no_ci', 'This repository has no CI')),
        row('Checks to ignore', plain(p.ignored_checks), lines('ignored_checks', 'in')),
        row('Review bots', plain(p.review_bots), lines('review_bots', 'in')),
      ]),
    ],
    [
      'Remove',
      <>
        <p className="small">Its tasks stay and still show its name. A project with an open task cannot be removed.</p>
        <button className="btn danger" onClick={onRemove}>
          Remove project
        </button>
      </>,
    ],
  ]

  if (folded)
    return sections.map(([title, body]) => (
      <details className="acc" key={title}>
        <summary>{title}</summary>
        <section>{body}</section>
      </details>
    ))
  const column = (part: [string, ReactNode][]) => (
    <div className="sections">
      {part.map(([title, body]) => (
        <section key={title}>
          <h3>{title}</h3>
          {body}
        </section>
      ))}
    </div>
  )
  return (
    <div className="two">
      {column(sections.slice(0, 4))}
      {column(sections.slice(4))}
    </div>
  )
}
