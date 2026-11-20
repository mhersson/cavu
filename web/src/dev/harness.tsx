// Development only: renders a screen from fixtures, with no server. Vite builds index.html alone.
import { useEffect } from 'react'
import { createRoot } from 'react-dom/client'
import { MemoryRouter } from 'react-router'
import '../theme/tokens.css'
import '../theme/base.css'
import App from '../App.tsx'
import { useLook } from '../theme/theme.ts'
import type { Look } from '../theme/theme.ts'
import { EVENTS, HEALTH, PROJECTS, ROWS, SETTINGS, VIEWS } from './fixtures.ts'

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

function answer(method: string, url: URL): Response {
  const path = url.pathname
  const q = url.searchParams
  if (method === 'POST' && (path === '/api/auth/login' || path === '/api/auth/logout')) {
    return new Response(null, { status: 204 })
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
    let rows = ROWS.filter(
      (r) =>
        (q.get('needs_you') !== 'true' || r.needs_you) &&
        (!q.get('project') || r.project === q.get('project')) &&
        (!q.get('status') || r.status === q.get('status')),
    )
    if (q.get('order') === 'newest') rows = [...rows].reverse()
    if (q.get('limit')) rows = rows.slice(0, Number(q.get('limit')))
    return json({ tasks: rows })
  }
  const task = path.match(/^\/api\/tasks\/(\d+)(\/events)?$/)
  if (task) {
    const id = Number(task[1])
    if (task[2]) return json({ events: EVENTS[id] ?? [] })
    return VIEWS[id] ? json(VIEWS[id]) : json({ error: 'not found' }, 404)
  }
  return json({ error: 'not found' }, 404)
}

window.fetch = (input, init) => {
  const url = new URL(String(input), window.location.origin)
  return Promise.resolve(answer(init?.method ?? 'GET', url))
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

const query = new URLSearchParams(window.location.search)
const look: Look = {
  theme: ['dark', 'light'].includes(query.get('theme') ?? '') ? (query.get('theme') as Look['theme']) : 'system',
  palette: query.get('palette') === 'catppuccin' ? 'catppuccin' : 'copper',
}

export function Apply() {
  const [, setLook] = useLook()
  useEffect(() => setLook(look), [setLook])
  return null
}

createRoot(document.getElementById('root')!).render(
  <MemoryRouter initialEntries={[query.get('path') ?? '/']}>
    <Apply />
    <App />
  </MemoryRouter>,
)
