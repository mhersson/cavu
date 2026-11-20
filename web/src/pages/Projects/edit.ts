import type { Project, Settings } from '../../api/types.ts'

// What the fields hold while a project is edited: a list or a map as its lines, an entry to a line.
export type Form = {
  repo_url: string
  base_branch: string
  setup_command: string
  verify_command: string
  verify_waived: boolean
  tool_overrides: string
  egress_hosts: string
  env: string
  caps: Record<string, string>
  switches: Record<string, boolean>
  forge_credential: string
  no_ci: boolean
  ignored_checks: string
  review_bots: string
  removed: string[]
  added: { name: string; value: string }[]
}

export type Edit = {
  form: Form
  set: <K extends keyof Form>(key: K, value: Form[K]) => void
  credentials: Settings['forge_credentials']
}

export const pairs = (m: Record<string, string>) => Object.entries(m).map(([k, v]) => `${k}=${v}`)

export const formOf = (p: Project): Form => ({
  repo_url: p.repo_url,
  base_branch: p.base_branch,
  setup_command: p.setup_command,
  verify_command: p.verify_command,
  verify_waived: p.verify_waived,
  tool_overrides: pairs(p.tool_overrides).join('\n'),
  egress_hosts: p.egress_hosts.join('\n'),
  env: pairs(p.env).join('\n'),
  caps: p.caps,
  switches: p.switches,
  forge_credential: p.forge_credential,
  no_ci: p.no_ci,
  ignored_checks: p.ignored_checks.join('\n'),
  review_bots: p.review_bots.join('\n'),
  removed: [],
  added: [],
})

// A name may hold spaces and commas, so only a line break separates entries.
const listOf = (text: string) =>
  text
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l !== '')

// A map's line is NAME=value, split at the first =.
const mapOf = (text: string): Record<string, string> =>
  Object.fromEntries(
    listOf(text).map((l) => {
      const [k, ...v] = l.split('=')
      return [k, v.join('=')]
    }),
  )

const sameMap = (a: Record<string, string>, b: Record<string, string>) =>
  Object.keys(a).length === Object.keys(b).length && Object.entries(a).every(([k, v]) => b[k] === v)

// The body of a PATCH: what differs from the project as it was when the edit began, and per key for what merges per key.
export function changes(was: Project, f: Form): Record<string, unknown> {
  const body: Record<string, unknown> = {}
  for (const k of ['repo_url', 'base_branch', 'setup_command', 'verify_command', 'verify_waived', 'forge_credential', 'no_ci'] as const)
    if (f[k] !== was[k]) body[k] = f[k]
  for (const k of ['egress_hosts', 'ignored_checks', 'review_bots'] as const) {
    const v = listOf(f[k])
    if (JSON.stringify(v) !== JSON.stringify(was[k])) body[k] = v
  }
  for (const k of ['tool_overrides', 'env'] as const) {
    const v = mapOf(f[k])
    if (!sameMap(v, was[k])) body[k] = v
  }
  const merged = {
    switches: Object.fromEntries(Object.entries(f.switches).filter(([k, v]) => v !== was.switches[k])),
    caps: Object.fromEntries(Object.entries(f.caps).filter(([k, v]) => v !== (was.caps[k] ?? ''))),
    // A removed name is sent empty, so an added row goes only whole: an empty value would remove the secret.
    secrets: Object.fromEntries([
      ...f.removed.map((n) => [n, '']),
      ...f.added.filter((s) => s.name && s.value).map((s) => [s.name, s.value]),
    ]),
  }
  for (const [k, m] of Object.entries(merged)) if (Object.keys(m).length > 0) body[k] = m
  return body
}
