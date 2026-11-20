import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { useEffect } from 'react'
import { MemoryRouter, useLocation } from 'react-router'
import App from '../../App.tsx'
import type { Project } from '../../api/types.ts'
import { FakeEventSource, fakeServer, json, narrow, type Call } from '../../test-server.ts'

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

function project(id: number, name: string, more: Partial<Project> = {}): Project {
  return {
    id,
    name,
    repo_url: `https://github.com/acme/${name}.git`,
    base_branch: 'main',
    setup_command: 'npm ci',
    verify_command: 'npm test',
    verify_waived: false,
    tool_overrides: {},
    egress_hosts: [],
    env: {},
    secret_vars: [],
    switches: {},
    caps: {},
    no_ci: false,
    ignored_checks: [],
    review_bots: [],
    forge_credential: '',
    runnable: true,
    baseline: { commit: 'c0ffee1', passed: true, at: '2030-01-02T03:04:05Z', output: '' },
    check: {
      requested_at: null,
      tools: [],
      ignored_tools: [],
      failed_step: '',
      verify_suggestion: '',
      fault: '',
      commit: 'c0ffee1',
      output: '',
    },
    ...more,
  }
}

const stuck = (id: number, name: string, check: Partial<Project['check']> = {}) => {
  const p = project(id, name, { runnable: false, baseline: null })
  return { ...p, check: { ...p.check, ...check } }
}

// The server answers GET /api/projects from world, which a test changes between reads; more answers the rest.
function serve(world: { projects: Project[] }, more: (c: Call) => Response | undefined = () => undefined) {
  return fakeServer((c) => {
    if (c.path === '/api/tasks?needs_you=true') return json({ tasks: [] }) // the frame's count
    if (c.method === 'GET' && c.path === '/api/projects') return json({ projects: world.projects })
    return more(c)
  })
}

// The address the app is at, for a test to read.
let here = ''
function Here() {
  const { pathname } = useLocation()
  useEffect(() => {
    here = pathname
  })
  return null
}

async function show(path = '/projects', phone = false) {
  vi.useFakeTimers()
  narrow(phone)
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
      <Here />
    </MemoryRouter>,
  )
  await act(() => vi.advanceTimersByTimeAsync(0))
}

const settle = () => act(() => vi.advanceTimersByTimeAsync(0))
const rows = () => within(screen.getByRole('navigation', { name: 'Projects' })).getAllByRole('link')
const current = () => rows().filter((r) => r.getAttribute('aria-current') === 'page').map((r) => r.textContent)
const reads = (calls: Call[]) => calls.filter((c) => c.method === 'GET' && c.path.startsWith('/api/projects'))
const deletes = (calls: Call[]) => calls.filter((c) => c.method === 'DELETE').map((c) => c.path)
const check = () => screen.getByRole('region', { name: 'Environment check' })
// The key and value list a value is in, as its keys with their values.
const pairs = (value: string): [string | null, HTMLElement][] => {
  const dl = screen.getByText(value).closest('dl')!
  const values = within(dl).getAllByRole('definition')
  return within(dl).getAllByRole('term').map((k, i) => [k.textContent, values[i]])
}

test('the list shows each project with a square by whether it can run', async () => {
  serve({ projects: [project(1, 'api'), stuck(2, 'docs'), project(3, 'web')] })
  await show()
  expect(
    rows().map((r) => [r.textContent, r.getAttribute('href'), r.classList.contains('k-ready'), r.classList.contains('k-stop')]),
  ).toEqual([
    ['api', '/projects/api', true, false],
    ['docs', '/projects/docs', false, true],
    ['web', '/projects/web', true, false],
  ])
  for (const r of rows()) expect(r.querySelector('.mk')).not.toBeNull()
})

test('the first project is the one beside the list, and the address chooses another', async () => {
  serve({ projects: [project(1, 'api'), project(2, 'web')] })
  await show()
  expect(current()).toEqual(['api'])
  expect(screen.getByRole('heading', { name: 'api' })).toBeInTheDocument()
  expect(screen.getByText('https://github.com/acme/api.git', { selector: '.repo' })).toBeInTheDocument()
  expect(screen.queryByRole('heading', { name: 'web' })).toBeNull()
  cleanup()

  serve({ projects: [project(1, 'api'), project(2, 'web')] })
  await show('/projects/web')
  expect(current()).toEqual(['web'])
  expect(screen.getByRole('heading', { name: 'web' })).toBeInTheDocument()
  expect(screen.queryByRole('heading', { name: 'api' })).toBeNull()
  cleanup()

  serve({ projects: [project(1, 'api'), project(2, 'web')] })
  await show('/projects/gone')
  expect(rows()).toHaveLength(2)
  expect(current()).toEqual([])
  expect(screen.queryByRole('heading', { level: 2 })).toBeNull()
  expect(screen.queryByRole('region', { name: 'Environment check' })).toBeNull()
})

test('choosing a row shows that project', async () => {
  const calls = serve({ projects: [project(1, 'api'), project(2, 'web')] })
  await show()
  fireEvent.click(rows()[1])
  await settle()
  expect(here).toBe('/projects/web')
  expect(current()).toEqual(['web'])
  expect(screen.getByRole('heading', { name: 'web' })).toBeInTheDocument()
  expect(reads(calls).map((c) => c.path)).toEqual(['/api/projects'])
})

test('a secret shows its name and never a value', async () => {
  serve({ projects: [project(1, 'web', { secret_vars: ['NPM_TOKEN', 'SENTRY_DSN'], env: { CI: '1' } })] })
  await show()
  const secrets = screen.getByText('NPM_TOKEN').closest('dd')!
  expect([...secrets.querySelectorAll('code')].map((c) => c.textContent)).toEqual(['NPM_TOKEN', 'SENTRY_DSN'])
  expect(screen.getByText('CI=1').tagName).toBe('CODE')
})

test('every switch is listed with its state', async () => {
  // Not in an order a sort would give, so the order served is the order shown.
  const switches = { web_tools: false, continuations: true, publish: false, readiness: true }
  serve({ projects: [project(1, 'web', { switches })] })
  await show()
  const shown = pairs('web_tools').map(([k, v]) => [k, v.textContent, v.querySelector('.small') !== null])
  expect(shown).toEqual([
    ['web_tools', 'off', true],
    ['continuations', 'on', false],
    ['publish', 'off', true],
    ['readiness', 'on', false],
  ])
  expect(screen.getByText('web_tools').tagName).toBe('CODE')
})

test('a project no check has ended for has its tools found told apart from none found', async () => {
  serve({ projects: [stuck(1, 'new', { commit: '' }), project(2, 'old')] })
  await show()
  const found = () => screen.getByText('Found by the check').nextElementSibling!
  const never = found().textContent
  expect(found().querySelector('.small')).not.toBeNull()
  fireEvent.click(rows()[1])
  await settle()
  expect(found().textContent).not.toBe(never)
})

test('an event reads the projects again', async () => {
  const world = { projects: [project(1, 'web')] }
  const calls = serve(world)
  await show()
  world.projects = [project(1, 'web'), project(2, 'zed')]
  act(() => FakeEventSource.all[0].send(1, { task_id: 1 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  expect(reads(calls)).toHaveLength(2)
  expect(rows().map((r) => r.textContent)).toEqual(['web', 'zed'])
})

test('a check that ends shows without a reload', async () => {
  const asked = stuck(1, 'web', { requested_at: '2030-01-02T03:00:00Z', commit: '' })
  const calls = serve({ projects: [asked] }, (c) => (c.method === 'GET' && c.path === '/api/projects/1' ? json(project(1, 'web')) : undefined))
  await show()
  expect(rows()[0]).toHaveClass('k-stop')
  expect(check()).toHaveClass('k-stop')
  await act(() => vi.advanceTimersByTimeAsync(3000))
  expect(reads(calls).map((c) => c.path)).toEqual(['/api/projects', '/api/projects/1'])
  expect(rows()[0]).toHaveClass('k-ready')
  expect(check()).toHaveClass('k-ready')
})

test('removing asks first, and a refusal names the open tasks', async () => {
  const line = 'project web has open tasks: 7, 9'
  const calls = serve({ projects: [project(1, 'web')] }, (c) => (c.method === 'DELETE' ? json({ error: line }, 400) : undefined))
  await show()
  fireEvent.click(screen.getByRole('button', { name: 'Remove project' }))
  const dialog = screen.getByRole('dialog', { name: /web/ })
  expect(deletes(calls)).toEqual([])

  const remove = within(dialog).getByRole('button', { name: 'Remove project' })
  fireEvent.click(remove)
  expect(remove).toBeDisabled()
  await settle()
  expect(deletes(calls)).toEqual(['/api/projects/1'])
  expect(within(screen.getByRole('dialog')).getByRole('alert').textContent).toBe(line)
  expect(remove).toBeEnabled()

  // It stays usable: removing again asks the server again.
  fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove project' }))
  await settle()
  expect(deletes(calls)).toHaveLength(2)
  expect(rows().map((r) => r.textContent)).toEqual(['web'])
})

test('a removed project leaves the list', async () => {
  const world = { projects: [project(1, 'api'), project(2, 'web')] }
  const calls = serve(world, (c) => {
    if (c.method !== 'DELETE' || c.path !== '/api/projects/2') return undefined
    world.projects = [project(1, 'api')]
    return new Response(null, { status: 204 })
  })
  await show('/projects/web')
  fireEvent.click(screen.getByRole('button', { name: 'Remove project' }))
  fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove project' }))
  await settle()
  expect(deletes(calls)).toEqual(['/api/projects/2'])
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(here).toBe('/projects')
  expect(reads(calls)).toHaveLength(2)
  expect(rows().map((r) => r.textContent)).toEqual(['api'])
})

test('on a phone the list is flat and a project folds its sections', async () => {
  serve({ projects: [project(1, 'api'), stuck(2, 'docs'), project(3, 'web')] })
  await show('/projects', true)
  const links = rows()
  expect(links.map((l) => [l.getAttribute('href'), l.classList.contains('k-ready'), l.querySelector('.mk') !== null])).toEqual([
    ['/projects/api', true, true],
    ['/projects/docs', false, true],
    ['/projects/web', true, true],
  ])
  // The line under each name says whether it can run: the same for both that can, another for the one that cannot.
  const label = (l: HTMLElement) => l.querySelector('.t2')!.textContent
  expect(label(links[0])).toBe(label(links[2]))
  expect(label(links[1])).not.toBe(label(links[0]))
  expect(screen.queryByRole('region', { name: 'Environment check' })).toBeNull()

  fireEvent.click(links[1])
  await settle()
  expect(here).toBe('/projects/docs')
  expect(screen.getByRole('link', { name: 'Projects' })).toHaveAttribute('href', '/projects')
  expect(screen.getByRole('heading', { level: 1, name: 'docs' })).toBeInTheDocument()
  expect(screen.queryByRole('navigation', { name: 'Projects' })).toBeNull()
  expect(check()).toHaveClass('k-stop')
  const folds = document.querySelectorAll('details')
  expect(folds).toHaveLength(8)
  expect([...folds].filter((d) => d.open)).toEqual([])
})
