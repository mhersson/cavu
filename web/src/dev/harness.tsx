// Development only: renders a screen from fixtures, with no server. Vite builds index.html alone.
import { useEffect } from 'react'
import { createRoot } from 'react-dom/client'
import { MemoryRouter } from 'react-router'
import '../theme/tokens.css'
import '../theme/base.css'
import App from '../App.tsx'
import { api } from '../api/client.ts'
import { useLook } from '../theme/theme.ts'
import type { Look } from '../theme/theme.ts'
import { EVENTS, HEALTH, PROJECTS, ROWS, SETTINGS, SETUP, VIEWS } from './fixtures.ts'

const query = new URLSearchParams(window.location.search)
// empty=1 serves no task that needs a person.
const rows = query.get('empty') === '1' ? ROWS.filter((r) => !r.needs_you) : ROWS

// images=0 serves every task without its images.
if (query.get('images') === '0') for (const v of Object.values(VIEWS)) v.images = []

// mode=key sets the endpoint mode to key, so the settings page shows its key-mode rows.
if (query.get('mode') === 'key') SETTINGS.settings.endpoint_mode = 'key'

// push=off|on|install-first|denied|unsupported makes this browser look so; the state is set every time, as one profile serves many pages.
const push = query.get('push')
if (push) {
  if (push === 'on') localStorage.setItem('push_device', '1')
  else localStorage.removeItem('push_device')
  if (push === 'denied') Object.defineProperty(window, 'Notification', { value: { permission: 'denied' }, configurable: true })
  if (push === 'unsupported' || push === 'install-first') Reflect.deleteProperty(window, 'PushManager')
  if (push === 'install-first') Object.defineProperty(navigator, 'standalone', { value: false, configurable: true })
}

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

// order=newest and limit, as the server pages a list; a limit of 0 is all of it.
function page<T>(list: T[], q: URLSearchParams): T[] {
  return (q.get('order') === 'newest' ? [...list].reverse() : list).slice(0, Number(q.get('limit')) || undefined)
}

function answer(method: string, url: URL, body: string): Response | undefined {
  const path = url.pathname
  const q = url.searchParams
  if (method === 'POST' && (path === '/api/auth/login' || path === '/api/auth/logout')) {
    return new Response(null, { status: 204 })
  }
  if (method === 'POST' && path === '/api/tasks') {
    const t = JSON.parse(body) as { project: string; title: string; body: string }
    return json({ ...VIEWS[42], id: 45, project: t.project, title: t.title, body: t.body, status: 'draft' }, 201)
  }
  // An upload is never answered, so its tile stays under way.
  if (method === 'POST' && /^\/api\/tasks\/\d+\/images$/.test(path)) return undefined
  const acted = path.match(/^\/api\/tasks\/(\d+)\/actions$/)
  if (method === 'POST' && acted && VIEWS[Number(acted[1])]) return json(VIEWS[Number(acted[1])])

  // PATCH /api/tasks/{id}: the view with the sent fields; a dependency takes its row's status, and one with no row is refused.
  const edited = path.match(/^\/api\/tasks\/(\d+)$/)
  if (method === 'PATCH' && edited && VIEWS[Number(edited[1])]) {
    const view = VIEWS[Number(edited[1])]
    const e = JSON.parse(body) as { title?: string; body?: string; depends_on?: number[]; remove_images?: number[] }
    const dependencies = e.depends_on?.map((d) => ({ id: d, status: ROWS.find((r) => r.id === d)?.status }))
    const none = dependencies?.find((d) => !d.status)
    if (none) return json({ error: `task ${none.id} does not exist` }, 400)
    view.images = view.images.filter((i) => !e.remove_images?.includes(i.id))
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

// lapsed=1 ends the access a moment after the page loads: every later request is redirected, and one is made, as the next screen's would be.
let lapsed = false
if (query.get('lapsed') === '1') {
  setTimeout(() => {
    lapsed = true
    api('GET', '/health').catch(() => {})
  }, 500)
}

window.fetch = (input, init) => {
  if (lapsed) return Promise.resolve(Object.defineProperties(new Response(null), { type: { value: 'opaqueredirect' }, status: { value: 0 } }))
  const url = new URL(String(input), window.location.origin)
  const got = answer(init?.method ?? 'GET', url, String(init?.body ?? ''))
  return got ? Promise.resolve(got) : new Promise<Response>(() => {})
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

// click=TEXT clicks the first button, link or summary whose text, or label when it has no text, starts with TEXT, once the page shows it.
const click = query.get('click')
if (click) {
  let tries = 0
  const timer = setInterval(() => {
    const target = [...document.querySelectorAll<HTMLElement>('button, a, summary')].find((e) => (e.textContent || e.getAttribute('aria-label'))?.startsWith(click))
    target?.click()
    target?.scrollIntoView()
    if (target || ++tries === 20) clearInterval(timer)
  }, 100)
}

// The screenshots the harness serves for a task's images, each in one look whatever the page's, as a picture keeps its
// colours: an odd id is a profile page in Copper light whose Saved label covers its Save button, an even id a settings
// panel in Catppuccin dark that cuts its values off. Drawn from the tokens.
function picture(id: number): string {
  const probe = document.createElement('div')
  probe.setAttribute(id % 2 ? 'data-theme' : 'data-palette', id % 2 ? 'light' : 'catppuccin')
  document.body.append(probe)
  const style = getComputedStyle(probe)
  const c = (token: string) => style.getPropertyValue('--' + token).trim()
  const svg = (w: number, h: number, inner: string) =>
    `<svg xmlns="http://www.w3.org/2000/svg" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" font-family="sans-serif" font-size="20" fill="${c('fg')}">` +
    `<rect width="${w}" height="${h}" fill="${c('bg-dim')}"/>${inner}</svg>`
  const line = (x1: number, y1: number, x2: number, y2: number) => `<line x1="${x1}" y1="${y1}" x2="${x2}" y2="${y2}" stroke="${c('bg3')}" stroke-width="2"/>`
  const text = (x: number, y: number, s: string, more = '') => `<text x="${x}" y="${y}" ${more}>${s}</text>`
  const grey = `fill="${c('grey1')}"`
  const field = (y: number, h: number, s: string) =>
    `<rect x="350" y="${y}" width="500" height="${h}" rx="8" fill="${c('bg0')}" stroke="${c('bg4')}" stroke-width="2"/>` + text(368, y + 30, s)
  const out =
    id % 2
      ? svg(1280, 800,
          `<clipPath id="w"><rect x="80" y="56" width="1120" height="688" rx="12"/></clipPath><g clip-path="url(#w)">` +
          `<rect x="80" y="56" width="1120" height="688" fill="${c('bg0')}"/><rect x="80" y="56" width="1120" height="52" fill="${c('bg1')}"/>` +
          `<rect x="80" y="108" width="230" height="636" fill="${c('bg1')}"/>${line(80, 108, 1200, 108)}${line(310, 108, 310, 744)}</g>` +
          `<rect x="80" y="56" width="1120" height="688" rx="12" fill="none" stroke="${c('bg3')}" stroke-width="2"/>` +
          `<circle cx="110" cy="82" r="7" fill="${c('red')}"/><circle cx="134" cy="82" r="7" fill="${c('yellow')}"/><circle cx="158" cy="82" r="7" fill="${c('green')}"/>` +
          text(640, 89, 'Storefront admin', `text-anchor="middle" ${grey}`) +
          ['Orders', 'Products', 'Customers'].map((s, i) => text(112, 160 + 44 * i, s, grey)).join('') +
          `<rect x="96" y="268" width="198" height="40" rx="8" fill="${c('bg3')}"/>${text(112, 295, 'Profile', 'font-weight="600"')}${text(112, 340, 'Settings', grey)}` +
          text(350, 172, 'Profile', 'font-size="30" font-weight="600"') +
          text(350, 222, 'Name', grey) + field(236, 46, 'Ada Lovelace') + text(350, 322, 'Email', grey) + field(336, 46, 'ada@example.com') +
          text(350, 422, 'About you', grey) + field(436, 110, 'Writes notes on the analytical engine.') +
          `<rect x="350" y="582" width="132" height="48" rx="8" fill="${c('blue')}"/>${text(416, 613, 'Save', `text-anchor="middle" font-weight="600" fill="${c('bg0')}"`)}` +
          `<rect x="372" y="586" width="196" height="38" rx="6" fill="${c('bg2')}"/>${text(388, 612, 'Saved 2 min ago', grey)}` +
          `<ellipse cx="460" cy="606" rx="150" ry="54" fill="none" stroke="${c('red')}" stroke-width="4"/>`)
      : svg(960, 640,
          `<rect x="60" y="46" width="840" height="548" rx="12" fill="${c('bg0')}" stroke="${c('bg3')}" stroke-width="2"/>` +
          text(96, 104, 'Settings', 'font-size="30" font-weight="600"') + text(96, 150, 'General', 'font-weight="600"') +
          `<rect x="96" y="160" width="80" height="3" rx="1.5" fill="${c('aqua')}"/>${text(212, 150, 'Notifications', grey)}${text(362, 150, 'Billing', grey)}${line(60, 164, 900, 164)}` +
          [
            ['Display name', 'Ada Lovelace, Countess of Lovelace'],
            ['Summary email', "Every weekday morning at seven o'clock"],
            ['Language', 'English (United Kingdom), with metric units'],
          ]
            .map(([k, v], i) => {
              const y = 196 + 80 * i
              return `<clipPath id="c${i}"><rect x="470" y="${y}" width="380" height="46" rx="8"/></clipPath>${text(96, y + 30, k)}` +
                `<rect x="470" y="${y}" width="380" height="46" rx="8" fill="${c('bg1')}" stroke="${c('bg3')}" stroke-width="2"/>${text(486, y + 30, v, `clip-path="url(#c${i})"`)}`
            })
            .join('') +
          text(96, 466, 'Show prices with tax') + `<rect x="790" y="444" width="60" height="32" rx="16" fill="${c('green')}"/><circle cx="834" cy="460" r="12" fill="${c('bg0')}"/>` +
          line(60, 512, 900, 512) + `<rect x="740" y="532" width="110" height="40" rx="8" fill="${c('bg2')}"/>${text(795, 558, 'Close', 'text-anchor="middle"')}` +
          `<rect x="770" y="186" width="96" height="226" rx="12" fill="none" stroke="${c('red')}" stroke-width="4"/>`)
  probe.remove()
  return 'data:image/svg+xml,' + encodeURIComponent(out)
}

// An image's address is the server's, which the harness has not: the browser is given the picture instead, both where
// React sets the attribute and where it sets the property again once mounted.
const shown = (value: string) => {
  const image = /^\/api\/tasks\/\d+\/images\/(\d+)$/.exec(value)
  return image ? picture(Number(image[1])) : value
}
const setAttribute = Element.prototype.setAttribute
Element.prototype.setAttribute = function (name: string, value: string) {
  setAttribute.call(this, name, name === 'src' ? shown(value) : value)
}
const src = Object.getOwnPropertyDescriptor(HTMLImageElement.prototype, 'src')!
Object.defineProperty(HTMLImageElement.prototype, 'src', {
  ...src,
  set(value: string) {
    src.set!.call(this, shown(value))
  },
})

// attach=image.png|shot.heic|big.png gives the page's image picker that file once it shows one: a screenshot, which the
// harness never finishes uploading, an iPhone's photo of a type CAVU does not keep, or a screenshot over the cap.
// drag=1 drags a file over the page.
const files: Record<string, [string, number]> = { 'image.png': ['image/png', 219136], 'shot.heic': ['image/heic', 2_400_000], 'big.png': ['image/png', 14_890_000] }
const attaching = files[query.get('attach') ?? '']
const dragging = query.get('drag') === '1'
if (attaching || dragging) {
  const dt = new DataTransfer()
  dt.items.add(new File([new Uint8Array(attaching?.[1] ?? 4)], query.get('attach') ?? 'image.png', { type: attaching?.[0] ?? 'image/png' }))
  let tries = 0
  const timer = setInterval(() => {
    const picker = document.querySelector<HTMLInputElement>('input[type=file]:not([capture])')
    const page = document.querySelector('.task-page')
    if (attaching && picker) {
      picker.files = dt.files
      picker.dispatchEvent(new Event('change', { bubbles: true }))
    }
    if (dragging && page) page.dispatchEvent(new DragEvent('dragover', { bubbles: true, cancelable: true, dataTransfer: dt }))
    if ((attaching ? picker : page) || ++tries === 20) clearInterval(timer)
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
