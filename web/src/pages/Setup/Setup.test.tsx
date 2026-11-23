import { cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import App from '../../App.tsx'
import type { Project, Settings } from '../../api/types.ts'
import { FakeEventSource, fakeServer, json, type Call } from '../../test-server.ts'

afterEach(() => vi.unstubAllGlobals())

const unset = { endpoint_credential: false, forge_token: false, forge_app_key: false }

function saved(settings: Settings['settings'] = {}, secrets: Partial<typeof unset> = {}, forge_credentials: Settings['forge_credentials'] = []): Settings {
  return { settings, secrets: { ...unset, ...secrets }, defaults: {}, switches: {}, forge_credentials, push_key: 'KEY', image_max_bytes: 0 }
}

const ENDPOINT = { endpoint_mode: 'subscription', model_work: 'sonnet' }
const COMMITS = { commit_name: 'Ada', commit_email: 'ada@example.com' }
const READY = saved({ ...ENDPOINT, ...COMMITS }, { endpoint_credential: true, forge_token: true })

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
    check: { requested_at: null, started_at: null, tools: [], ignored_tools: [], failed_step: '', verify_suggestion: '', fault: '', commit: 'c0ffee1', output: '' },
    ...more,
  }
}

const stuck = (id: number, name: string, check: Partial<Project['check']> = {}) => {
  const p = project(id, name, { runnable: false, baseline: null })
  return { ...p, check: { ...p.check, ...check } }
}

// The server answers the two reads from world, which a test changes between them; more answers the rest.
function serve(world: { settings: Settings; projects: Project[] }, more: (c: Call) => Response | undefined = () => undefined) {
  return fakeServer((c) => {
    if (c.method === 'GET' && c.path === '/api/settings') return json(world.settings)
    if (c.method === 'GET' && c.path === '/api/projects') return json({ projects: world.projects })
    return more(c)
  })
}

// A PATCH of the settings answers the next of answers, which the world then holds.
function serveSaves(world: { settings: Settings; projects: Project[] }, answers: (Settings | Response)[], more: (c: Call) => Response | undefined = () => undefined) {
  return serve(world, (c) => {
    if (c.method !== 'PATCH' || c.path !== '/api/settings') return more(c)
    const next = answers.shift()!
    if (next instanceof Response) return next
    world.settings = next
    return json(next)
  })
}

async function show() {
  render(
    <MemoryRouter initialEntries={['/setup']}>
      <App />
    </MemoryRouter>,
  )
  await screen.findByRole('region', { name: 'The model endpoint' })
  return userEvent.setup()
}

const step = (name: string) => screen.getByRole('region', { name })
const current = () => screen.getAllByRole('region').filter((r) => r.getAttribute('aria-current') === 'step')
const writes = (calls: Call[]) => calls.filter((c) => c.method !== 'GET').map((c) => [c.method, c.path, c.body])
const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement

const PEM = '-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----\n'

test('an instance with no credential for the model opens at the first step', async () => {
  serve({ settings: saved({ ...ENDPOINT, ...COMMITS }, { forge_token: true }), projects: [project(1, 'web')] })
  await show()
  expect(current()).toEqual([step('The model endpoint')])
  expect(screen.getByRole('radio', { name: 'A subscription' })).toHaveAttribute('aria-checked', 'true')
  expect(field("The subscription's token")).toHaveAttribute('type', 'password')
  expect(field("The subscription's token")).toHaveValue('')
  expect(field('Work model')).toHaveValue('sonnet')
  // The later steps are done, and the fourth waits for the first.
  expect(within(step('GitHub')).getByRole('button', { name: 'Change' })).toBeInTheDocument()
  expect(within(step('The first project')).getByRole('link', { name: 'Change' })).toHaveAttribute('href', '/projects/web')
  expect(screen.queryByRole('region', { name: 'Environment check' })).toBeNull()
  cleanup()

  // A fresh instance chooses no mode and asks no secret until one is chosen.
  serve({ settings: saved(), projects: [] })
  await show()
  expect(current()).toEqual([step('The model endpoint')])
  expect(screen.getAllByRole('radio').map((r) => r.getAttribute('aria-checked'))).toEqual(['false', 'false'])
  expect(document.querySelector('input[type=password]')).toBeNull()
  expect(within(step('GitHub')).queryByRole('button')).toBeNull()
  expect(within(step('The first project')).queryByRole('link')).toBeNull()
})

test('the page has the bar and no navigation, tabs or event stream', async () => {
  serve({ settings: saved(), projects: [] })
  await show()
  expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument()
  expect(screen.queryByRole('navigation')).toBeNull()
  expect(screen.queryByRole('link')).toBeNull()
  expect(FakeEventSource.all).toEqual([])
})

test('each step saves and opens the next', async () => {
  const world = { settings: saved(), projects: [] as Project[] }
  const made = stuck(9, 'fresh', { requested_at: '2030-01-02T03:00:00Z', commit: '' })
  const calls = serveSaves(
    world,
    [saved(ENDPOINT, { endpoint_credential: true }), READY],
    (c) => {
      // Another project, made on the command line meanwhile, comes first.
      if (c.method === 'POST' && c.path === '/api/projects') {
        world.projects = [project(1, 'api'), made]
        return json(made, 201)
      }
      if (c.method === 'POST' && c.path === '/api/projects/9/check') return json(made, 202)
    },
  )
  const user = await show()

  await user.click(screen.getByRole('radio', { name: 'A subscription' }))
  await user.type(field("The subscription's token"), 'sk-sub')
  await user.type(field('Work model'), 'sonnet')
  await user.click(screen.getByRole('button', { name: 'Save and go on' }))
  await waitFor(() => expect(current()).toEqual([step('GitHub')]))
  expect(within(step('The model endpoint')).getByRole('button', { name: 'Change' })).toBeInTheDocument()

  await user.type(field('A token for the repositories CAVU works on'), 'ghp-1')
  await user.type(field('Name on commits'), 'Ada')
  await user.type(field('Email on commits'), 'ada@example.com')
  await user.click(screen.getByRole('button', { name: 'Save and go on' }))
  await waitFor(() => expect(current()).toEqual([step('The first project')]))
  // No named credential, so the project takes the default and there is nothing to pick.
  expect(screen.queryByLabelText('GitHub credential')).toBeNull()

  await user.type(field('Name'), 'fresh')
  await user.type(field('Repository address'), 'https://github.com/acme/fresh.git')
  await user.click(screen.getByRole('button', { name: 'Create and check' }))
  await waitFor(() => expect(current()).toEqual([step('The environment check')]))
  expect(within(step('The environment check')).getByRole('region', { name: 'Environment check' })).toBeInTheDocument()
  expect(within(step('The first project')).getByRole('link', { name: 'Change' })).toHaveAttribute('href', '/projects/fresh')

  expect(writes(calls)).toEqual([
    ['PATCH', '/api/settings', { settings: { endpoint_mode: 'subscription', model_work: 'sonnet' }, secrets: { endpoint_credential: 'sk-sub' } }],
    [
      'PATCH',
      '/api/settings',
      { settings: { commit_name: 'Ada', commit_email: 'ada@example.com', forge_app_id: null }, secrets: { forge_token: 'ghp-1', forge_app_key: '' } },
    ],
    [
      'POST',
      '/api/projects',
      {
        name: 'fresh',
        repo_url: 'https://github.com/acme/fresh.git',
        base_branch: 'main',
        forge_credential: '',
        setup_command: '',
        verify_command: '',
        verify_waived: false,
      },
    ],
    ['POST', '/api/projects/9/check', undefined],
  ])
  expect(calls.filter((c) => c.method === 'GET' && c.path === '/api/projects').length).toBeGreaterThanOrEqual(2)
})

test('the check\'s failure shows in the fourth step with "Check again"', async () => {
  serve({ settings: READY, projects: [stuck(1, 'web', { failed_step: 'setup', output: 'npm error code E401' })] })
  await show()
  const fourth = within(step('The environment check'))
  expect(current()).toEqual([step('The environment check')])
  expect(within(fourth.getByRole('region', { name: 'Environment check' })).getByRole('button', { name: 'Check again' })).toBeInTheDocument()
  expect(fourth.getByRole('link', { name: 'Open the project' })).toHaveAttribute('href', '/projects/web')
  expect(fourth.queryByRole('button', { name: 'Create the first task' })).toBeNull()
})

test('step 4 shows no button of its own while a check is asked for', async () => {
  const world = { settings: READY, projects: [stuck(1, 'web', { failed_step: 'setup' })] }
  const calls = serve(world, (c) =>
    c.method === 'POST' && c.path === '/api/projects/1/check' ? json(stuck(1, 'web', { failed_step: 'setup', requested_at: '2030-01-02T03:00:00Z' }), 202) : undefined,
  )
  const user = await show()
  const fourth = within(step('The environment check'))
  await user.click(fourth.getByRole('button', { name: 'Check again' }))
  await waitFor(() => expect(fourth.queryByRole('link', { name: 'Open the project' })).toBeNull())
  expect(fourth.queryByRole('button', { name: 'Create the first task' })).toBeNull()
  expect(fourth.getByRole('region', { name: 'Environment check' })).toBeInTheDocument()
  expect(writes(calls)).toEqual([['POST', '/api/projects/1/check', undefined]])
})

test('the first step says that cavu doctor tests the endpoint', async () => {
  serve({ settings: saved(), projects: [] })
  await show()
  const commands = [...step('The model endpoint').querySelectorAll('code')].map((c) => c.textContent)
  expect(commands).toContain('cavu doctor')
})

test('a finished setup offers the first task', async () => {
  serve({ settings: READY, projects: [stuck(1, 'api'), project(2, 'web')] })
  const user = await show()
  expect(current()).toEqual([step('The environment check')])
  // The first project that can run is the one shown.
  expect(within(step('The first project')).getByRole('link', { name: 'Change' })).toHaveAttribute('href', '/projects/web')
  expect(screen.getByRole('region', { name: 'Environment check' })).toHaveClass('k-ready')
  expect(screen.queryByRole('link', { name: 'Open the project' })).toBeNull()
  await user.click(screen.getByRole('button', { name: 'Create the first task' }))
  expect(screen.getByRole('dialog', { name: 'New task' })).toBeInTheDocument()
})

test('the open last step links to the settings for notifications', async () => {
  serve({ settings: READY, projects: [project(2, 'web')] })
  await show()
  expect(within(step('The environment check')).getByRole('link', { name: 'Settings' })).toHaveAttribute('href', '/settings')
})

test("a refused save shows the server's line and keeps what was typed", async () => {
  const line = 'setting model_work: not a model'
  const calls = serveSaves({ settings: saved(), projects: [] }, [json({ error: line }, 400), json({ error: line }, 400)])
  const user = await show()
  await user.click(screen.getByRole('radio', { name: 'A subscription' }))
  await user.type(field("The subscription's token"), 'sk-sub')
  await user.type(field('Work model'), 'nope')
  await user.click(screen.getByRole('button', { name: 'Save and go on' }))
  expect((await within(step('The model endpoint')).findByRole('alert')).textContent).toBe(line)
  expect(current()).toEqual([step('The model endpoint')])
  expect(field("The subscription's token")).toHaveValue('sk-sub')
  expect(field('Work model')).toHaveValue('nope')
  expect(screen.getByRole('radio', { name: 'A subscription' })).toHaveAttribute('aria-checked', 'true')
  await user.click(screen.getByRole('button', { name: 'Save and go on' }))
  await waitFor(() => expect(writes(calls)).toHaveLength(2))
})

test('"Change" opens a done step with its saved values and sends no secret left empty', async () => {
  const calls = serveSaves({ settings: READY, projects: [project(1, 'web')] }, [saved({ ...ENDPOINT, ...COMMITS, model_work: 'opus' }, { endpoint_credential: true, forge_token: true })])
  const user = await show()
  await user.click(within(step('The model endpoint')).getByRole('button', { name: 'Change' }))
  expect(current()).toEqual([step('The model endpoint')])
  expect(screen.getByRole('radio', { name: 'A subscription' })).toHaveAttribute('aria-checked', 'true')
  expect(field('Work model')).toHaveValue('sonnet')
  // The secret is set: its field is empty and says so.
  expect(field("The subscription's token")).toHaveValue('')
  expect(field("The subscription's token")).toHaveAttribute('placeholder')
  await user.clear(field('Work model'))
  await user.type(field('Work model'), 'opus')
  await user.click(screen.getByRole('button', { name: 'Save and go on' }))
  await waitFor(() => expect(current()).toEqual([step('The environment check')]))
  expect(writes(calls)).toEqual([['PATCH', '/api/settings', { settings: { model_work: 'opus' } }]])
})

test('a Save with nothing typed sends no request', async () => {
  const calls = serve({ settings: READY, projects: [project(1, 'web')] })
  const user = await show()
  await user.click(within(step('GitHub')).getByRole('button', { name: 'Change' }))
  expect(current()).toEqual([step('GitHub')])
  expect(field('Name on commits')).toHaveValue('Ada')
  expect(field('A token for the repositories CAVU works on')).toHaveValue('')
  await user.click(screen.getByRole('button', { name: 'Save and go on' }))
  expect(current()).toEqual([step('The environment check')])
  expect(writes(calls)).toEqual([])
})

test('key mode sends the address and the header', async () => {
  const calls = serveSaves({ settings: saved(), projects: [] }, [saved({ ...ENDPOINT, endpoint_mode: 'key' }, { endpoint_credential: true })])
  const user = await show()
  await user.click(screen.getByRole('radio', { name: 'A key and an endpoint' }))
  expect(screen.queryByLabelText("The subscription's token")).toBeNull()
  await user.type(field('Endpoint address'), 'https://gateway.example/v1')
  await user.type(field('Auth header'), 'x-api-key')
  expect(field('The key')).toHaveAttribute('type', 'password')
  await user.type(field('The key'), 'sk-key')
  await user.type(field('Work model'), 'sonnet')
  await user.click(screen.getByRole('button', { name: 'Save and go on' }))
  await waitFor(() => expect(writes(calls)).toHaveLength(1))
  expect(writes(calls)).toEqual([
    [
      'PATCH',
      '/api/settings',
      {
        settings: { endpoint_mode: 'key', model_work: 'sonnet', endpoint_url: 'https://gateway.example/v1', endpoint_auth_header: 'x-api-key' },
        secrets: { endpoint_credential: 'sk-key' },
      },
    ],
  ])
  // The answer leaves the step open, and the key it sent is no longer held.
  expect(current()).toEqual([step('The model endpoint')])
  await waitFor(() => expect(field('The key')).toHaveValue(''))
})

test("an App's key is read from its file and sent", async () => {
  // A saved App id opens the App's form first.
  const world = { settings: saved({ ...ENDPOINT, forge_app_id: '7' }, { endpoint_credential: true }), projects: [] as Project[] }
  const calls = serveSaves(world, [saved({ ...ENDPOINT, ...COMMITS, forge_app_id: '8' }, { endpoint_credential: true, forge_app_key: true })])
  const user = await show()
  expect(current()).toEqual([step('GitHub')])
  expect(field('App id')).toHaveValue('7')
  expect(screen.queryByLabelText('A token for the repositories CAVU works on')).toBeNull()

  // The way back to a token, and to the App again.
  await user.click(screen.getByRole('button', { name: 'Use a token instead' }))
  expect(screen.queryByLabelText('App id')).toBeNull()
  await user.click(screen.getByRole('button', { name: 'Use a GitHub App instead' }))

  await user.clear(field('App id'))
  await user.type(field('App id'), '8')
  expect(field('Private key')).toHaveAttribute('accept', '.pem')
  await user.upload(field('Private key'), new File([PEM], 'app.pem', { type: 'application/x-pem-file' }))
  await user.type(field('Name on commits'), 'Ada')
  await user.type(field('Email on commits'), 'ada@example.com')
  await user.click(screen.getByRole('button', { name: 'Save and go on' }))
  await waitFor(() => expect(current()).toEqual([step('The first project')]))
  expect(writes(calls)).toEqual([
    [
      'PATCH',
      '/api/settings',
      { settings: { commit_name: 'Ada', commit_email: 'ada@example.com', forge_app_id: '8' }, secrets: { forge_app_key: PEM, forge_token: '' } },
    ],
  ])
})

test('an App id typed without its key file is sent', async () => {
  const world = { settings: saved(ENDPOINT, { endpoint_credential: true }), projects: [] as Project[] }
  const calls = serveSaves(world, [saved({ ...ENDPOINT, forge_app_id: '7' }, { endpoint_credential: true })])
  const user = await show()
  await user.click(screen.getByRole('button', { name: 'Use a GitHub App instead' }))
  await user.type(field('App id'), '7')
  await user.click(screen.getByRole('button', { name: 'Save and go on' }))
  await waitFor(() => expect(writes(calls)).toHaveLength(1))
  expect(writes(calls)).toEqual([['PATCH', '/api/settings', { settings: { forge_app_id: '7' } }]])
  // The step stays open, on the App's form, with the id as saved.
  expect(current()).toEqual([step('GitHub')])
  await waitFor(() => expect(field('App id')).toHaveValue('7'))
})

test("a failed read shows the server's line", async () => {
  const line = 'the store is closed'
  fakeServer((c) => (c.path === '/api/settings' ? json({ error: line }, 500) : c.path === '/api/projects' ? json({ projects: [] }) : undefined))
  render(
    <MemoryRouter initialEntries={['/setup']}>
      <App />
    </MemoryRouter>,
  )
  expect((await screen.findByRole('alert')).textContent).toBe(line)
})
