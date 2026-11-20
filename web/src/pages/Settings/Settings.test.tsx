import { cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { Settings as Saved } from '../../api/types.ts'
import { fakeServer, json, narrow, type Call } from '../../test-server.ts'
import Settings from './Settings.tsx'

afterEach(() => vi.unstubAllGlobals())

// Every settable key and its default, as the server serves them.
const DEFAULTS: Record<string, string> = {
  runtime: '', ca_bundle_path: '', endpoint_mode: '', endpoint_url: '', endpoint_auth_header: '',
  model_work: '', effort_work: '', model_review: '', effort_review: '', model_strong: '', effort_strong: '',
  task_timeout_minutes: '300', task_max_turns: '1000', task_budget_usd: '', daily_budget_usd: '', model_prices: '',
  verify_timeout_minutes: '10', commit_name: '', commit_email: '', concurrency: '3', listen_address: '127.0.0.1:7117',
  skills_path: '', instructions_path: '', forge_app_id: '', ci_timeout_minutes: '120',
}

const unset = { endpoint_credential: false, forge_token: false, forge_app_key: false }

const saved = (settings: Saved['settings'] = {}, secrets = unset): Saved => ({
  settings, secrets, defaults: DEFAULTS, switches: {}, forge_credentials: [],
})

const isPatch = (c: Call) => c.method === 'PATCH' && c.path === '/api/settings'
const patches = (calls: Call[]) => calls.filter(isPatch).map((c) => c.body)

type Used = { name: string; forge_credential: string }

function open(first: Saved, patch: () => Response = () => json(first), projects: Used[] = []) {
  const calls = fakeServer((c) => {
    if (c.path === '/api/projects') return json({ projects })
    if (c.path === '/api/settings') return c.method === 'GET' ? json(first) : patch()
  })
  render(<Settings />)
  return calls
}

// A section by its heading, once the settings are read.
const section = async (name: string) => (await screen.findByRole('heading', { name })).closest('section')!

const NAMED: Saved['forge_credentials'] = [
  { name: 'acme-org', kind: 'token' },
  { name: 'old', kind: 'token' },
  { name: 'work-app', kind: 'app', app_id: 482913 },
]

const PEM = '-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----\n'
const pem = () => new File([PEM], 'app.pem', { type: 'application/x-pem-file' })

// The credential table's rows, the default first, and a named one's row by its name.
const creds = async () => within(await section('GitHub')).getAllByRole('row') as HTMLTableRowElement[]
const cred = (name: string) => within(screen.getByRole('cell', { name }).closest('tr')!)
const names = (rows: HTMLTableRowElement[]) => rows.slice(1).map((r) => r.cells[0].textContent)

// The form's Add or Replace comes after the table's buttons.
const send = (name: string) => screen.getAllByRole('button', { name }).at(-1)!

test('every settable key the server lists has a field', async () => {
  const defaults = { ...DEFAULTS, queue_depth: '' }
  const settings: Saved['settings'] = Object.fromEntries(Object.keys(defaults).map((k) => [k, 'v-' + k]))
  settings.endpoint_mode = 'key'
  settings.model_prices = { opus: { input: 15 } }
  settings.forge_app_id = '482913'
  open({ ...saved(settings), defaults })
  await section('The server')
  for (const k of Object.keys(defaults)) {
    if (!['endpoint_mode', 'forge_app_id', 'model_prices'].includes(k)) expect(screen.getByDisplayValue('v-' + k)).toBeEnabled()
  }
  expect(screen.getByDisplayValue('{"opus":{"input":15}}')).toBeEnabled()
  expect(screen.getByLabelText('queue_depth')).toHaveValue('v-queue_depth')
  expect(within((await creds())[0]).getByText('482913')).toBeInTheDocument()
  expect(screen.queryByDisplayValue('482913')).toBeNull()
  expect(screen.getAllByRole('textbox')).toHaveLength(Object.keys(defaults).length - 2)
})

test('an unset key shows its default as a hint', async () => {
  open(saved({ task_max_turns: '40' }))
  const limits = within(await section('Limits'))
  expect(limits.getByLabelText('Tasks at once')).toHaveValue('')
  expect(limits.getByLabelText('Tasks at once')).toHaveAttribute('placeholder', '3')
  expect(screen.getByLabelText('Listens on')).toHaveAttribute('placeholder', '127.0.0.1:7117')
  expect(screen.getByLabelText('Work')).toHaveAttribute('placeholder')
  expect(limits.getByLabelText('A task, turns')).toHaveValue('40')
  expect(limits.getByLabelText('A task, dollars')).not.toHaveAttribute('placeholder')
})

test("Save sends its section's changed keys and null for an emptied one", async () => {
  const first = saved({ model_work: 'sonnet', model_review: 'sonnet', commit_name: 'cavu' })
  const calls = open(first, () => json(saved({ model_work: 'opus', effort_work: 'high', effort_strong: 'max', commit_name: 'cavu' })))
  const user = userEvent.setup()
  const models = within(await section('Models'))
  const commits = within(await section('Commits'))
  await user.clear(models.getByLabelText('Work'))
  await user.type(models.getByLabelText('Work'), 'opus')
  await user.type(models.getByLabelText('Work effort'), 'high')
  await user.clear(models.getByLabelText('Review'))
  await user.type(commits.getByLabelText('Name'), ' bot')
  await user.click(models.getByRole('button', { name: 'Save' }))
  expect(await models.findByDisplayValue('max')).toBeInTheDocument()
  expect(patches(calls)).toEqual([{ settings: { model_work: 'opus', effort_work: 'high', model_review: null } }])
  expect(models.getByRole('button', { name: 'Save' })).toBeDisabled()
  expect(commits.getByLabelText('Name')).toHaveValue('cavu bot')
  expect(commits.getByRole('button', { name: 'Save' })).toBeEnabled()
})

test("a refused save shows the server's line", async () => {
  const refusal = 'setting concurrency: "many" is not a whole number of 1 or more'
  open(saved(), () => json({ error: refusal }, 400))
  const user = userEvent.setup()
  const limits = within(await section('Limits'))
  await user.type(limits.getByLabelText('Tasks at once'), 'many')
  await user.click(limits.getByRole('button', { name: 'Save' }))
  expect(await limits.findByRole('alert')).toHaveTextContent(refusal)
  expect(limits.getByLabelText('Tasks at once')).toHaveValue('many')
})

test('the dollar limits are off under a subscription', async () => {
  const calls = open(saved({ endpoint_mode: 'subscription', endpoint_url: 'https://gateway.example' }))
  const user = userEvent.setup()
  const endpoint = within(await section('Model endpoint'))
  const keyed = ['Endpoint address', 'Auth header', 'A task, dollars', 'A day, dollars', 'Model prices'].map((l) => screen.getByLabelText(l))
  for (const f of keyed) expect(f).toBeDisabled()
  expect(keyed[0]).toHaveValue('https://gateway.example')
  expect(screen.getByLabelText('Tasks at once')).toBeEnabled()
  await user.click(endpoint.getByRole('radio', { name: 'A key and an endpoint' }))
  for (const f of keyed) expect(f).toBeEnabled()
  expect(patches(calls)).toEqual([])
  await user.click(endpoint.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(patches(calls)).toEqual([{ settings: { endpoint_mode: 'key' } }]))
})

test('a secret is replaced without ever being shown', async () => {
  const calls = open(saved({}, { ...unset, endpoint_credential: true }))
  const user = userEvent.setup()
  const endpoint = within(await section('Model endpoint'))
  await user.click(endpoint.getByRole('button', { name: 'Replace' }))
  expect(endpoint.getByLabelText('Credential')).toHaveAttribute('type', 'password')
  await user.type(endpoint.getByLabelText('Credential'), 'sk-new-key')
  await user.click(endpoint.getByRole('button', { name: 'Save' }))
  expect(await endpoint.findByRole('button', { name: 'Replace' })).toBeInTheDocument()
  expect(patches(calls)).toEqual([{ secrets: { endpoint_credential: 'sk-new-key' } }])
  expect(screen.queryByDisplayValue('sk-new-key')).toBeNull()
  expect(document.body.innerHTML).not.toContain('sk-new-key')
})

test('a credential is added, replaced and removed', async () => {
  const [acme, old, app] = NAMED
  const added = { name: 'new-org', kind: 'token' } as const
  const moved = { name: 'work-app', kind: 'app', app_id: 99 } as const
  const after = (...forge_credentials: Saved['forge_credentials']) => json({ ...saved(), forge_credentials })
  const answers = [after(acme, added, old, app), after(acme, added, old, moved), after(acme, added, moved)]
  const calls = open({ ...saved(), forge_credentials: NAMED }, () => answers.shift()!, [{ name: 'web', forge_credential: 'acme-org' }])
  const user = userEvent.setup()
  const gh = within(await section('GitHub'))

  await user.click(gh.getByRole('button', { name: 'Add a credential' }))
  await user.type(gh.getByLabelText('Name'), 'new-org')
  await user.type(gh.getByLabelText('Token'), 'ghp-new')
  await user.click(gh.getByRole('button', { name: 'Add' }))
  await waitFor(() => expect(names(gh.getAllByRole('row') as HTMLTableRowElement[])).toEqual(['acme-org', 'new-org', 'old', 'work-app']))
  expect(gh.queryByLabelText('Name')).toBeNull()

  await user.click(cred('work-app').getByRole('button', { name: 'Replace' }))
  expect(gh.getByRole('radio', { name: 'A GitHub App' })).toHaveAttribute('aria-checked', 'true')
  expect(gh.getByLabelText('App id')).toHaveValue('482913')
  await user.clear(gh.getByLabelText('App id'))
  await user.type(gh.getByLabelText('App id'), '99')
  await user.upload(gh.getByLabelText('Private key'), pem())
  await user.click(send('Replace'))
  expect(await cred('work-app').findByText('99')).toBeInTheDocument()

  await user.click(cred('old').getByRole('button', { name: 'Remove' }))
  await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Remove' }))
  await waitFor(() => expect(names(gh.getAllByRole('row') as HTMLTableRowElement[])).toEqual(['acme-org', 'new-org', 'work-app']))
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(patches(calls)).toEqual([
    { forge_credentials: { 'new-org': { token: 'ghp-new' } } },
    { forge_credentials: { 'work-app': { app_id: 99, app_key: PEM } } },
    { forge_credentials: { old: null } },
  ])
})

test("the default credential's Replace clears the other kind", async () => {
  const answers = [json(saved({}, { ...unset, forge_token: true })), json(saved({ forge_app_id: '8' }, { ...unset, forge_app_key: true }))]
  const calls = open(saved({ forge_app_id: '7' }, { ...unset, forge_app_key: true }), () => answers.shift()!)
  const user = userEvent.setup()
  const gh = within(await section('GitHub'))
  const first = () => within(gh.getAllByRole('row')[0])

  await user.click(first().getByRole('button', { name: 'Replace' }))
  expect(gh.getByRole('radio', { name: 'A GitHub App' })).toHaveAttribute('aria-checked', 'true')
  await user.click(gh.getByRole('radio', { name: 'A token' }))
  await user.type(gh.getByLabelText('Token'), 'ghp-default')
  await user.click(send('Replace'))
  await waitFor(() => expect(gh.queryByLabelText('Token')).toBeNull())

  await user.click(first().getByRole('button', { name: 'Replace' }))
  expect(gh.getByRole('radio', { name: 'A token' })).toHaveAttribute('aria-checked', 'true')
  await user.click(gh.getByRole('radio', { name: 'A GitHub App' }))
  await user.type(gh.getByLabelText('App id'), '8')
  await user.upload(gh.getByLabelText('Private key'), pem())
  await user.click(send('Replace'))
  await waitFor(() => expect(gh.queryByLabelText('App id')).toBeNull())
  expect(patches(calls)).toEqual([
    { settings: { forge_app_id: null }, secrets: { forge_token: 'ghp-default', forge_app_key: '' } },
    { settings: { forge_app_id: '8' }, secrets: { forge_app_key: PEM, forge_token: '' } },
  ])
})

test("the default credential's row follows forge_app_id and the two secrets", async () => {
  const row = async (settings: Saved['settings'], secrets: typeof unset) => {
    cleanup()
    open(saved(settings, secrets))
    return within((await creds())[0])
  }

  let first = await row({ forge_app_id: '7' }, { ...unset, forge_token: true })
  expect(first.getByRole('code')).toHaveTextContent('7')
  expect(first.getByRole('button', { name: 'Replace' })).toBeInTheDocument()

  first = await row({}, { ...unset, forge_token: true, forge_app_key: true })
  expect(first.queryByRole('code')).toBeNull()
  expect(first.getByRole('button', { name: 'Replace' })).toBeInTheDocument()

  first = await row({}, { ...unset, forge_app_key: true })
  expect(first.queryByRole('code')).toBeNull()
  expect(first.getByRole('button', { name: 'Set' })).toBeInTheDocument()
  expect(first.queryByRole('button', { name: 'Replace' })).toBeNull()
})

test('Remove is absent for a credential a project uses', async () => {
  open({ ...saved(), forge_credentials: NAMED }, undefined, [{ name: 'web', forge_credential: 'acme-org' }])
  await section('GitHub')
  expect(await cred('old').findByRole('button', { name: 'Remove' })).toBeInTheDocument()
  expect(cred('work-app').getByRole('button', { name: 'Remove' })).toBeInTheDocument()
  expect(cred('acme-org').queryByRole('button', { name: 'Remove' })).toBeNull()
  expect(within((await creds())[0]).queryByRole('button', { name: 'Remove' })).toBeNull()
  expect(screen.getAllByRole('button', { name: 'Remove' })).toHaveLength(2)
})

test("a refused credential change shows the server's line", async () => {
  const refusal = 'credential "Acme": want 1 to 40 of a-z, 0-9 and -'
  open(saved(), () => json({ error: refusal }, 400))
  const user = userEvent.setup()
  const gh = within(await section('GitHub'))
  await user.click(gh.getByRole('button', { name: 'Add a credential' }))
  await user.type(gh.getByLabelText('Name'), 'Acme')
  await user.type(gh.getByLabelText('Token'), 'ghp-x')
  await user.click(gh.getByRole('button', { name: 'Add' }))
  expect(await gh.findByRole('alert')).toHaveTextContent(refusal)
  expect(gh.getByLabelText('Name')).toHaveValue('Acme')
  expect(gh.getByLabelText('Token')).toHaveValue('ghp-x')
})

test('a key is sent once and never shown', async () => {
  const calls = open({ ...saved(), forge_credentials: NAMED })
  const user = userEvent.setup()
  const gh = within(await section('GitHub'))
  await user.click(cred('work-app').getByRole('button', { name: 'Replace' }))
  await user.upload(gh.getByLabelText('Private key'), pem())
  expect(document.body.innerHTML).not.toContain('MIIEpAIBAAKCAQEA')
  await user.click(send('Replace'))
  await waitFor(() => expect(gh.queryByLabelText('Private key')).toBeNull())
  expect(patches(calls)).toEqual([{ forge_credentials: { 'work-app': { app_id: 482913, app_key: PEM } } }])
  expect(document.body.innerHTML).not.toContain('MIIEpAIBAAKCAQEA')
})

test('a credential is sent only with its secret, and an App with its id', async () => {
  const calls = open(saved())
  const user = userEvent.setup()
  const gh = within(await section('GitHub'))

  await user.click(gh.getByRole('button', { name: 'Add a credential' }))
  await user.type(gh.getByLabelText('Name'), 'new-org')
  expect(gh.getByRole('button', { name: 'Add' })).toBeDisabled()
  await user.type(gh.getByLabelText('Token'), 'ghp-new')
  expect(gh.getByRole('button', { name: 'Add' })).toBeEnabled()
  await user.click(gh.getByRole('radio', { name: 'A GitHub App' }))
  await user.type(gh.getByLabelText('App id'), '9')
  expect(gh.getByRole('button', { name: 'Add' })).toBeDisabled()
  await user.upload(gh.getByLabelText('Private key'), pem())
  expect(gh.getByRole('button', { name: 'Add' })).toBeEnabled()
  await user.click(gh.getByRole('radio', { name: 'A token' }))
  await user.click(gh.getByRole('radio', { name: 'A GitHub App' }))
  expect(gh.getByRole('button', { name: 'Add' })).toBeDisabled()
  await user.click(gh.getByRole('button', { name: 'Cancel' }))

  await user.click(within(gh.getAllByRole('row')[0]).getByRole('button', { name: 'Set' }))
  await user.click(gh.getByRole('radio', { name: 'A GitHub App' }))
  await user.upload(gh.getByLabelText('Private key'), pem())
  expect(send('Replace')).toBeDisabled()
  await user.type(gh.getByLabelText('App id'), '8')
  expect(send('Replace')).toBeEnabled()
  expect(patches(calls)).toEqual([])
})

test('a file chosen for one credential is never sent for another', async () => {
  open({ ...saved(), forge_credentials: [...NAMED, { name: 'ci-app', kind: 'app', app_id: 7 }] })
  const user = userEvent.setup()
  const gh = within(await section('GitHub'))
  await user.click(cred('work-app').getByRole('button', { name: 'Replace' }))
  await user.upload(gh.getByLabelText('Private key'), pem())
  expect((gh.getByLabelText('Private key') as HTMLInputElement).files).toHaveLength(1)
  await user.click(cred('ci-app').getByRole('button', { name: 'Replace' }))
  expect(gh.getByLabelText('App id')).toHaveValue('7')
  expect((gh.getByLabelText('Private key') as HTMLInputElement).files).toHaveLength(0)
  expect(send('Replace')).toBeDisabled()
})

test('a narrow window shows each section as a fold, the first open', async () => {
  narrow(true)
  open(saved())
  await waitFor(() => expect(screen.getAllByRole('group')).toHaveLength(7))
  const folds = screen.getAllByRole('group')
  expect(folds.map((f) => f.hasAttribute('open'))).toEqual([true, false, false, false, false, false, false])
  expect(folds.map((f) => within(f).queryAllByRole('button', { name: 'Save', hidden: true }).length)).toEqual([1, 1, 1, 0, 1, 1, 0])
  expect(within(folds[3]).getAllByRole('table', { hidden: true })).toHaveLength(1)
  expect(within(folds[6]).getAllByRole('radiogroup', { hidden: true })).toHaveLength(2)
  expect(screen.queryByRole('navigation')).toBeNull()
  expect(screen.queryByRole('heading', { level: 2 })).toBeNull()
})

test('choosing a palette and a theme sets the root and the stored keys', async () => {
  open(saved())
  const user = userEvent.setup()
  await section('Model endpoint')
  const [palette, theme] = within(await section('This browser')).getAllByRole('radiogroup')
  const root = document.documentElement

  const radios = (g: HTMLElement) => within(g).getAllByRole('radio')
  expect(radios(palette).map((r) => r.getAttribute('aria-checked'))).toEqual(['true', 'false'])

  await user.click(radios(palette)[1])
  expect(root.dataset.palette).toBe('catppuccin')
  expect(localStorage.getItem('palette')).toBe('catppuccin')
  expect(radios(palette).map((r) => r.getAttribute('aria-checked'))).toEqual(['false', 'true'])

  await user.click(radios(theme)[2])
  expect(root.dataset.theme).toBe('light')
  expect(localStorage.getItem('theme')).toBe('light')
  expect(radios(theme).map((r) => r.getAttribute('aria-checked'))).toEqual(['false', 'false', 'true'])

  await user.click(radios(theme)[1])
  expect(root.dataset.theme).toBe('dark')
  expect(localStorage.getItem('theme')).toBe('dark')

  await user.click(radios(theme)[0])
  expect(localStorage.getItem('theme')).toBeNull()
  expect(localStorage.getItem('palette')).toBe('catppuccin')
})
