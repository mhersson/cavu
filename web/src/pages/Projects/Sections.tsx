import { Fragment, type ReactNode } from 'react'
import type { Project } from '../../api/types.ts'

type Props = { project: Project; folded: boolean; onRemove: () => void }

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

const pairs = (m: Record<string, string>) => Object.entries(m).map(([k, v]) => `${k}=${v}`)

function kv(rows: [ReactNode, ReactNode][]) {
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

// A desktop lays the sections in two columns; a phone folds each.
export default function Sections({ project: p, folded, onRemove }: Props) {
  const cap = (key: string) => p.caps[key] || small("the instance's setting")
  const checked = p.check.fault !== '' || p.check.failed_step !== '' || p.check.commit !== ''
  const sections: [string, ReactNode][] = [
    ['Repository', kv([['Address', mono(p.repo_url)], ['Base branch', mono(p.base_branch)]])],
    [
      'Commands',
      kv([
        ['Setup', mono(p.setup_command)],
        ['Verify', p.verify_waived ? 'Waived: a person checks every change.' : mono(p.verify_command)],
      ]),
    ],
    [
      'Tools',
      kv([
        ['Found by the check', checked ? plain(p.check.tools) : small('not checked yet')],
        ['Versions set here', list(pairs(p.tool_overrides))],
      ]),
    ],
    [
      'The sandbox',
      kv([
        ['Hosts it may reach', list(p.egress_hosts, 'the package registries of its languages')],
        ['Environment', list(pairs(p.env))],
        [
          'Secrets',
          p.secret_vars.length > 0 ? (
            <>
              {list(p.secret_vars)} {small('set; a value is never shown')}
            </>
          ) : (
            small('none')
          ),
        ],
      ]),
    ],
    [
      "Limits for this project's tasks",
      kv([
        ['Minutes', cap('task_timeout_minutes')],
        ['Turns', cap('task_max_turns')],
        ['Dollars', cap('task_budget_usd')],
      ]),
    ],
    ['Steps', kv(Object.entries(p.switches).map(([name, on]) => [<code>{name}</code>, on ? 'on' : small('off')]))],
    [
      'GitHub',
      kv([
        ['Credential', p.forge_credential ? <code>{p.forge_credential}</code> : 'the default'],
        ['Checks', p.no_ci ? 'This repository has no CI.' : 'Watched after every push.'],
        ['Checks to ignore', plain(p.ignored_checks)],
        ['Review bots', plain(p.review_bots)],
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
