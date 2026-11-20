// Development only: renders a screen from fixtures, with no server. Vite builds index.html alone.
import { useEffect } from 'react'
import { createRoot } from 'react-dom/client'
import { MemoryRouter } from 'react-router'
import '../theme/tokens.css'
import '../theme/base.css'
import App from '../App.tsx'
import { useLook } from '../theme/theme.ts'
import type { Look } from '../theme/theme.ts'
import { EVENTS, HEALTH, PROJECTS, ROWS, SETTINGS, SETUP, VIEWS } from './fixtures.ts'

const query = new URLSearchParams(window.location.search)
// empty=1 serves no task that needs a person.
const rows = query.get('empty') === '1' ? ROWS.filter((r) => !r.needs_you) : ROWS

// mode=key sets the endpoint mode to key, so the settings page shows its key-mode rows.
if (query.get('mode') === 'key') SETTINGS.settings.endpoint_mode = 'key'

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

// order=newest and limit, as the server pages a list; a limit of 0 is all of it.
function page<T>(list: T[], q: URLSearchParams): T[] {
  return (q.get('order') === 'newest' ? [...list].reverse() : list).slice(0, Number(q.get('limit')) || undefined)
}

function answer(method: string, url: URL, body: string): Response {
  const path = url.pathname
  const q = url.searchParams
  if (method === 'POST' && (path === '/api/auth/login' || path === '/api/auth/logout')) {
    return new Response(null, { status: 204 })
  }
  if (method === 'POST' && path === '/api/tasks') {
    const t = JSON.parse(body) as { project: string; title: string; body: string }
    return json({ ...VIEWS[42], id: 45, project: t.project, title: t.title, body: t.body, status: 'draft' }, 201)
  }
  const acted = path.match(/^\/api\/tasks\/(\d+)\/actions$/)
  if (method === 'POST' && acted && VIEWS[Number(acted[1])]) return json(VIEWS[Number(acted[1])])

  // PATCH /api/tasks/{id}: the view with the sent fields; a dependency takes its row's status, and one with no row is refused.
  const edited = path.match(/^\/api\/tasks\/(\d+)$/)
  if (method === 'PATCH' && edited && VIEWS[Number(edited[1])]) {
    const view = VIEWS[Number(edited[1])]
    const e = JSON.parse(body) as { title?: string; body?: string; depends_on?: number[] }
    const dependencies = e.depends_on?.map((d) => ({ id: d, status: ROWS.find((r) => r.id === d)?.status }))
    const none = dependencies?.find((d) => !d.status)
    if (none) return json({ error: `task ${none.id} does not exist` }, 400)
    return json({ ...view, title: e.title ?? view.title, body: e.body ?? view.body, dependencies: dependencies ?? view.dependencies })
  }

  if (method !== 'GET') return json({ error: 'not found' }, 404)
  if (path === '/api/health') return json(HEALTH)
  if (path === '/api/settings') return json(SETTINGS)
  if (path === '/api/projects') return json({ projects: PROJECTS })
  const project = path.match(/^\/api\/projects\/([^/]+)$/)
  if (project) {
    const p = PROJECTS.find((x) => String(x.id) === project[1] || x.name === project[1])
    return p ? json(p) : json({ error: 'not found' }, 404)
  }
  if (path === '/api/tasks') {
    const statuses = q.get('status')?.split(',')
    const kept = rows.filter(
      (r) =>
        (q.get('needs_you') !== 'true' || r.needs_you) &&
        (!q.get('project') || r.project === q.get('project')) &&
        (!statuses || statuses.includes(r.status)),
    )
    return json({ tasks: page(kept, q) })
  }
  const task = path.match(/^\/api\/tasks\/(\d+)(\/events)?$/)
  if (task) {
    const id = Number(task[1])
    if (task[2]) {
      // after=ID: the events above it, or below it under order=newest
      const after = Number(q.get('after'))
      const newest = q.get('order') === 'newest'
      const events = (EVENTS[id] ?? []).filter(
        (e) => (q.get('agent') !== 'false' || e.actor !== 'agent') && (!after || (newest ? e.id < after : e.id > after)),
      )
      return json({ events: page(events, q) })
    }
    return VIEWS[id] ? json(VIEWS[id]) : json({ error: 'not found' }, 404)
  }
  return json({ error: 'not found' }, 404)
}

window.fetch = (input, init) => {
  const url = new URL(String(input), window.location.origin)
  return Promise.resolve(answer(init?.method ?? 'GET', url, String(init?.body ?? '')))
}

// A stream that opens and stays live.
class LiveEventSource {
  static CLOSED = 2
  readyState = 1
  onopen: (() => void) | null = null
  constructor() {
    setTimeout(() => this.onopen?.(), 0)
  }
  close() {
    this.readyState = LiveEventSource.CLOSED
  }
}
window.EventSource = LiveEventSource as unknown as typeof EventSource

const look: Look = {
  theme: ['dark', 'light'].includes(query.get('theme') ?? '') ? (query.get('theme') as Look['theme']) : 'system',
  palette: query.get('palette') === 'catppuccin' ? 'catppuccin' : 'copper',
}

export function Apply() {
  const [, setLook] = useLook()
  useEffect(() => setLook(look), [setLook])
  return null
}

// click=TEXT clicks the first button or link whose text starts with TEXT, once the page shows it.
const click = query.get('click')
if (click) {
  let tries = 0
  const timer = setInterval(() => {
    const target = [...document.querySelectorAll<HTMLElement>('button, a')].find((e) => e.textContent?.startsWith(click))
    target?.click()
    if (target || ++tries === 20) clearInterval(timer)
  }, 100)
}

// setup=1|2|3|4|failed serves a fresh instance at that point of its setup.
const setup = SETUP[query.get('setup') ?? '']
if (setup) {
  Object.assign(SETTINGS, { settings: setup.settings, secrets: setup.secrets, forge_credentials: [] })
  PROJECTS.splice(0, PROJECTS.length, ...setup.projects)
}

createRoot(document.getElementById('root')!).render(
  <MemoryRouter initialEntries={[query.get('path') ?? '/']}>
    <Apply />
    <App />
  </MemoryRouter>,
)
