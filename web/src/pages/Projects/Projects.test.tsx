import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { useEffect } from 'react'
import { MemoryRouter, useLocation } from 'react-router'
import App from '../../App.tsx'
import type { Project, Settings } from '../../api/types.ts'
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
// The page's reads of the projects: the frame's own read of the list, made once at mount, is left out.
const reads = (calls: Call[]) => {
  const all = calls.filter((c) => c.method === 'GET' && c.path.startsWith('/api/projects'))
  all.splice(all.findIndex((c) => c.path === '/api/projects'), 1)
  return all
}
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

// The new-project dialog: the settings it reads and the requests Create sends.
const CREDENTIALS = [
  { name: 'acme-org', kind: 'token' as const },
  { name: 'work-app', kind: 'app' as const, app_id: 7 },
]
function serveNew(world: { projects: Project[] }, refuse?: Response) {
  return serve(world, (c) => {
    if (c.method === 'GET' && c.path === '/api/settings') return json({ forge_credentials: CREDENTIALS })
    if (c.method === 'POST' && c.path === '/api/projects') {
      if (refuse) return refuse
      const made = project(9, 'fresh', { runnable: false, baseline: null })
      world.projects = [...world.projects, made]
      return json(made, 201)
    }
    if (c.method === 'POST' && c.path === '/api/projects/9/check') return json(world.projects.find((p) => p.id === 9))
    return undefined
  })
}
const posts = (calls: Call[]) => calls.filter((c) => c.method === 'POST').map((c) => [c.path, c.body])
const typeInto = (name: string, value: string) => fireEvent.change(within(screen.getByRole('dialog')).getByLabelText(name), { target: { value } })
const openNew = async () => {
  fireEvent.click(screen.getByRole('button', { name: 'New project' }))
  await settle()
}

test('the new-project dialog creates and opens the project', async () => {
  const calls = serveNew({ projects: [project(1, 'api')] })
  await show()
  await openNew()
  typeInto('Name', 'fresh')
  typeInto('Repository address', 'https://github.com/acme/fresh.git')
  typeInto('Setup command', 'make deps')
  typeInto('Verify command', 'make test')
  fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Create' }))
  await settle()
  expect(posts(calls)).toEqual([
    [
      '/api/projects',
      {
        name: 'fresh',
        repo_url: 'https://github.com/acme/fresh.git',
        base_branch: 'main',
        forge_credential: '',
        setup_command: 'make deps',
        verify_command: 'make test',
        verify_waived: false,
      },
    ],
    ['/api/projects/9/check', undefined],
  ])
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(here).toBe('/projects/fresh')
  expect(rows().map((r) => r.textContent)).toEqual(['api', 'fresh'])
  expect(current()).toEqual(['fresh'])
  expect(screen.getByRole('heading', { name: 'fresh' })).toBeInTheDocument()
})

test('the credential picker lists the default and the named ones', async () => {
  const calls = serveNew({ projects: [project(1, 'api')] })
  await show()
  await openNew()
  const pick = within(screen.getByRole('dialog')).getByLabelText('GitHub credential') as HTMLSelectElement
  expect([...pick.options].map((o) => o.value)).toEqual(['', 'acme-org', 'work-app'])
  expect(pick.value).toBe('')
  fireEvent.change(pick, { target: { value: 'work-app' } })
  typeInto('Name', 'fresh')
  fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Create' }))
  await settle()
  expect((posts(calls)[0][1] as { forge_credential: string }).forge_credential).toBe('work-app')
})

test('the credential picker shows only when a named credential exists', async () => {
  const cases: [Settings['forge_credentials'], boolean][] = [
    [[], false],
    [[CREDENTIALS[0]], true],
  ]
  for (const [credentials, shown] of cases) {
    serve({ projects: [project(1, 'api')] }, (c) => (c.method === 'GET' && c.path === '/api/settings' ? json({ forge_credentials: credentials }) : undefined))
    await show()
    await openNew()
    expect(within(screen.getByRole('dialog')).queryByLabelText('GitHub credential') !== null).toBe(shown)
    cleanup()
  }
})

test('a refused project shows the server\'s line and keeps what was typed', async () => {
  const line = 'a project named fresh exists'
  const calls = serveNew({ projects: [project(1, 'api')] }, json({ error: line }, 409))
  await show()
  await openNew()
  typeInto('Name', 'fresh')
  typeInto('Verify command', 'make test')
  fireEvent.click(within(screen.getByRole('dialog')).getByLabelText('No verify command: a person checks every change'))
  const create = within(screen.getByRole('dialog')).getByRole('button', { name: 'Create' })
  fireEvent.click(create)
  expect(create).toBeDisabled()
  await settle()
  expect(within(screen.getByRole('dialog')).getByRole('alert').textContent).toBe(line)
  expect(posts(calls).map((p) => p[0])).toEqual(['/api/projects'])
  const d = within(screen.getByRole('dialog'))
  expect((d.getByLabelText('Name') as HTMLInputElement).value).toBe('fresh')
  expect((d.getByLabelText('Verify command') as HTMLInputElement).value).toBe('make test')
  expect((d.getByLabelText('Base branch') as HTMLInputElement).value).toBe('main')
  expect(d.getByLabelText('No verify command: a person checks every change')).toBeChecked()
  expect(create).toBeEnabled()
  expect(here).toBe('/projects')
})

test('the tick sends verify_waived', async () => {
  const calls = serveNew({ projects: [project(1, 'api')] })
  await show()
  await openNew()
  typeInto('Name', 'fresh')
  const tick = within(screen.getByRole('dialog')).getByLabelText('No verify command: a person checks every change')
  expect(tick).not.toBeChecked()
  fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Create' }))
  await settle()
  fireEvent.click(screen.getByRole('button', { name: 'New project' }))
  await settle()
  typeInto('Name', 'fresh')
  fireEvent.click(within(screen.getByRole('dialog')).getByLabelText('No verify command: a person checks every change'))
  fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Create' }))
  await settle()
  expect(posts(calls).filter((p) => p[0] === '/api/projects').map((p) => (p[1] as { verify_waived: boolean }).verify_waived)).toEqual([false, true])
})

test('Cancel sends nothing', async () => {
  const calls = serveNew({ projects: [project(1, 'api')] })
  await show()
  await openNew()
  typeInto('Name', 'fresh')
  fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }))
  expect(screen.queryByRole('dialog')).toBeNull()
  await openNew()
  fireEvent.keyDown(document, { key: 'Escape' })
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(calls.filter((c) => c.method !== 'GET')).toEqual([])
})

test('a phone shows no New project button', async () => {
  serveNew({ projects: [project(1, 'api')] })
  await show('/projects', true)
  expect(screen.queryByRole('button', { name: 'New project' })).toBeNull()
})

describe('editing', () => {
  // GET /api/settings names the two credentials; answer answers a PATCH of project 1, and checked a check request.
  function serveEdit(world: { projects: Project[] }, answer: (c: Call) => Response, checked?: Project) {
    return serve(world, (c) => {
      if (c.method === 'GET' && c.path === '/api/settings') return json({ forge_credentials: CREDENTIALS })
      if (c.method === 'PATCH' && c.path === '/api/projects/1') return answer(c)
      if (c.method === 'POST' && c.path === '/api/projects/1/check' && checked) return json(checked, 202)
      return undefined
    })
  }
  const patches = (calls: Call[]) => calls.filter((c) => c.method === 'PATCH').map((c) => [c.path, c.body])
  const writes = (calls: Call[]) => calls.filter((c) => c.method !== 'GET')
  const field = (name: string) => screen.getByRole('textbox', { name }) as HTMLTextAreaElement
  const write = (name: string, value: string) => fireEvent.change(field(name), { target: { value } })
  const press = async (name: string) => {
    fireEvent.click(screen.getByRole('button', { name }))
    await settle()
  }
  const editing = () => screen.queryByRole('button', { name: 'Save' }) !== null
  const WAIVED = 'No verify command: a person checks every change'

  test('Edit is on the desktop and not on a phone', async () => {
    serve({ projects: [project(1, 'web')] })
    await show('/projects/web')
    expect(screen.getByRole('button', { name: 'Edit' })).toBeInTheDocument()
    cleanup()
    serve({ projects: [project(1, 'web')] })
    await show('/projects/web', true)
    expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
  })

  test('Save sends only what changed', async () => {
    const was = project(1, 'web', {
      switches: { continuations: true, publish: true, web_tools: false },
      caps: { task_timeout_minutes: '60', task_max_turns: '40' },
    })
    const saved = { ...was, verify_command: 'make test' }
    const calls = serveEdit({ projects: [was] }, () => json(saved))
    await show()
    await press('Edit')
    expect(editing()).toBe(true)
    write('Verify', 'make test')
    fireEvent.click(screen.getByRole('checkbox', { name: 'publish' }))
    write('Dollars', '5')
    write('Turns', '')
    await press('Save')
    expect(patches(calls)).toEqual([
      ['/api/projects/1', { verify_command: 'make test', switches: { publish: false }, caps: { task_budget_usd: '5', task_max_turns: '' } }],
    ])
    // The project the server gave back is the one shown, and editing has ended.
    expect(editing()).toBe(false)
    expect(screen.getByText('make test').tagName).toBe('CODE')
  })

  test('nothing changed sends nothing', async () => {
    const world = { projects: [project(1, 'web', { env: { A: '1', B: '2' } })] }
    const calls = serveEdit(world, () => json(world.projects[0]))
    await show()
    await press('Edit')
    // A map's lines in another order, and a value typed and typed back.
    write('Environment', 'B=2\nA=1')
    write('Verify', 'make test')
    write('Verify', 'npm test')
    // A read while editing leaves the fields as typed, and the edit is against the project as it was.
    world.projects = [project(1, 'web', { env: { A: '1', B: '2' }, verify_command: 'go test ./...' })]
    act(() => FakeEventSource.all[0].send(1, { task_id: 1 }))
    await act(() => vi.advanceTimersByTimeAsync(2000))
    expect(reads(calls)).toHaveLength(2)
    expect(field('Verify').value).toBe('npm test')
    await press('Save')
    expect(writes(calls)).toEqual([])
    expect(editing()).toBe(false)
  })

  test("a refused save shows the server's line and keeps what was typed", async () => {
    const line = 'verify_command and verify_waived: a project may not have both'
    const calls = serveEdit({ projects: [project(1, 'web')] }, () => json({ error: line }, 400))
    await show()
    await press('Edit')
    write('Verify', 'make test')
    write('Hosts it may reach', 'a.example\nb.example')
    fireEvent.click(screen.getByRole('checkbox', { name: WAIVED }))
    const save = screen.getByRole('button', { name: 'Save' })
    fireEvent.click(save)
    expect(save).toBeDisabled()
    await settle()
    expect(screen.getByRole('alert').textContent).toBe(line)
    expect(editing()).toBe(true)
    expect(field('Verify').value).toBe('make test')
    expect(field('Hosts it may reach').value).toBe('a.example\nb.example')
    expect(screen.getByRole('checkbox', { name: WAIVED })).toBeChecked()
    expect(save).toBeEnabled()
    fireEvent.click(save)
    await settle()
    expect(patches(calls)).toHaveLength(2)
  })

  test('a secret shows its name and never a value', async () => {
    const value = 'hunter2-not-shown'
    const calls = serveEdit({ projects: [project(1, 'web', { secret_vars: ['NPM_TOKEN', 'SENTRY_DSN'] })] }, () =>
      json(project(1, 'web', { secret_vars: ['NPM_TOKEN', 'API_KEY'] })),
    )
    await show()
    const secrets = () => screen.getByText('SENTRY_DSN').closest('dd')!
    const note = secrets().querySelector('.small')!.textContent
    await press('Edit')
    // Editing keeps the read view's note beside the names.
    expect([...secrets().querySelectorAll('.small')].map((s) => s.textContent)).toEqual([note])
    // Remove marks a name, and the mark can be taken back; each button names its secret.
    const npm = within(secrets()).getByRole('button', { name: 'Remove NPM_TOKEN' })
    fireEvent.click(npm)
    expect(npm).toHaveAccessibleName('Keep NPM_TOKEN')
    fireEvent.click(npm)
    expect(npm).toHaveAccessibleName('Remove NPM_TOKEN')
    fireEvent.click(within(secrets()).getByRole('button', { name: 'Remove SENTRY_DSN' }))
    // Three added: one whole, one with a name and no value, one left empty; only the whole one is sent.
    for (let i = 0; i < 3; i++) fireEvent.click(screen.getByRole('button', { name: 'Add a secret' }))
    const names = screen.getAllByLabelText('Secret name')
    const values = screen.getAllByLabelText('Secret value')
    expect(names).toHaveLength(3)
    expect(values.map((v) => v.getAttribute('type'))).toEqual(['password', 'password', 'password'])
    fireEvent.change(names[0], { target: { value: 'API_KEY' } })
    fireEvent.change(values[0], { target: { value } })
    fireEvent.change(names[1], { target: { value: 'NPM_TOKEN' } })
    await press('Save')
    expect(patches(calls)).toEqual([['/api/projects/1', { secrets: { SENTRY_DSN: '', API_KEY: value } }]])
    expect(editing()).toBe(false)
    expect(screen.getByText('API_KEY')).toBeInTheDocument()
    expect(document.body.textContent).not.toContain(value)
    expect([...document.querySelectorAll('input')].map((i) => i.value)).not.toContain(value)
  })

  test('the credential picker lists the default and the named ones', async () => {
    const calls = serveEdit({ projects: [project(1, 'web', { forge_credential: 'acme-org' })] }, () =>
      json(project(1, 'web', { forge_credential: 'work-app' })),
    )
    await show()
    expect(calls.filter((c) => c.path === '/api/settings')).toEqual([])
    await press('Edit')
    expect(calls.filter((c) => c.path === '/api/settings')).toHaveLength(1)
    const pick = screen.getByRole('combobox', { name: 'Credential' }) as HTMLSelectElement
    expect([...pick.options].map((o) => o.value)).toEqual(['', 'acme-org', 'work-app'])
    expect(pick.value).toBe('acme-org')
    fireEvent.change(pick, { target: { value: 'work-app' } })
    await press('Save')
    expect(patches(calls)).toEqual([['/api/projects/1', { forge_credential: 'work-app' }]])
  })

  test('Save asks for a check when the project it gets back cannot run and has no baseline', async () => {
    const asked = '2030-01-02T04:00:00Z'
    const notPassed = { commit: 'c0ffee1', passed: false, at: '2030-01-02T03:04:05Z', output: '' }
    const cases: [Project, boolean][] = [
      [stuck(1, 'web'), true],
      [project(1, 'web'), false],
      [stuck(1, 'web', { requested_at: asked }), false],
      [{ ...stuck(1, 'web'), baseline: notPassed }, false],
    ]
    for (const [answer, asks] of cases) {
      const calls = serveEdit({ projects: [project(1, 'web')] }, () => json(answer), stuck(1, 'web', { requested_at: asked }))
      await show()
      await press('Edit')
      write('Verify', 'make test')
      await press('Save')
      expect(writes(calls).map((c) => [c.method, c.path])).toEqual([
        ['PATCH', '/api/projects/1'],
        ...(asks ? [['POST', '/api/projects/1/check']] : []),
      ])
      if (asks) {
        // The page took the 202's project, whose asked-for check it reads until it ends.
        await act(() => vi.advanceTimersByTimeAsync(3000))
        expect(reads(calls).map((c) => c.path)).toContain('/api/projects/1')
      }
      cleanup()
    }
  })

  test('lists are sent one entry per line', async () => {
    const calls = serveEdit({ projects: [project(1, 'web', { ignored_checks: ['lint'] })] }, () => json(project(1, 'web')))
    await show()
    await press('Edit')
    // A textarea holds a row per entry, at least one.
    expect(field('Hosts it may reach').rows).toBe(1)
    expect(field('Checks to ignore').rows).toBe(1)
    write('Hosts it may reach', 'a.example\nb.example')
    expect(field('Hosts it may reach').rows).toBe(2)
    write('Environment', 'A=b=c')
    write('Versions set here', 'go=1.24')
    write('Checks to ignore', 'lint\nci / test, fast')
    write('Review bots', 'copilot[bot]')
    await press('Save')
    expect(patches(calls)).toEqual([
      [
        '/api/projects/1',
        {
          egress_hosts: ['a.example', 'b.example'],
          env: { A: 'b=c' },
          tool_overrides: { go: '1.24' },
          ignored_checks: ['lint', 'ci / test, fast'],
          review_bots: ['copilot[bot]'],
        },
      ],
    ])
  })

  test('the ticks send verify_waived and no_ci and change no other field', async () => {
    const calls = serveEdit({ projects: [project(1, 'web')] }, () => json(project(1, 'web')))
    await show()
    await press('Edit')
    fireEvent.click(screen.getByRole('checkbox', { name: WAIVED }))
    fireEvent.click(screen.getByRole('checkbox', { name: 'This repository has no CI' }))
    expect(field('Verify')).toBeEnabled()
    expect(field('Verify').value).toBe('npm test')
    await press('Save')
    expect(patches(calls)).toEqual([['/api/projects/1', { verify_waived: true, no_ci: true }]])
  })

  test('Cancel drops what was typed', async () => {
    const calls = serveEdit({ projects: [project(1, 'web')] }, () => json(project(1, 'web')))
    await show()
    await press('Edit')
    write('Verify', 'make test')
    await press('Cancel')
    expect(editing()).toBe(false)
    expect(writes(calls)).toEqual([])
    expect(screen.getByText('npm test').tagName).toBe('CODE')
    await press('Edit')
    expect(field('Verify').value).toBe('npm test')
  })

  test('choosing another project leaves editing', async () => {
    serveEdit({ projects: [project(1, 'api'), project(2, 'web')] }, () => json(project(1, 'api')))
    await show()
    await press('Edit')
    write('Verify', 'make test')
    fireEvent.click(rows()[1])
    await settle()
    expect(editing()).toBe(false)
    fireEvent.click(rows()[0])
    await settle()
    expect(editing()).toBe(false)
    await press('Edit')
    expect(field('Verify').value).toBe('npm test')
  })
})
