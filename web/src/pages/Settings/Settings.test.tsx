import { render, screen, waitFor, within } from '@testing-library/react'
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

function open(first: Saved, patch: () => Response = () => json(first)) {
  const calls = fakeServer((c) => {
    if (c.path === '/api/settings') return c.method === 'GET' ? json(first) : patch()
  })
  render(<Settings />)
  return calls
}

// A section by its heading, once the settings are read.
const section = async (name: string) => (await screen.findByRole('heading', { name })).closest('section')!

test('every settable key the server lists has a field', async () => {
  const defaults = { ...DEFAULTS, queue_depth: '' }
  const settings: Saved['settings'] = Object.fromEntries(Object.keys(defaults).map((k) => [k, 'v-' + k]))
  settings.endpoint_mode = 'key'
  settings.model_prices = { opus: { input: 15 } }
  open({ ...saved(settings), defaults })
  await section('The server')
  for (const k of Object.keys(defaults)) {
    if (!['endpoint_mode', 'forge_app_id', 'model_prices'].includes(k)) expect(screen.getByDisplayValue('v-' + k)).toBeEnabled()
  }
  expect(screen.getByDisplayValue('{"opus":{"input":15}}')).toBeEnabled()
  expect(screen.getByLabelText('queue_depth')).toHaveValue('v-queue_depth')
  expect(screen.queryByDisplayValue('v-forge_app_id')).toBeNull()
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
  expect(calls).toHaveLength(1)
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

test('a narrow window shows each section as a fold, the first open', async () => {
  narrow(true)
  open(saved())
  await waitFor(() => expect(screen.getAllByRole('group')).toHaveLength(6))
  const folds = screen.getAllByRole('group')
  expect(folds.map((f) => f.hasAttribute('open'))).toEqual([true, false, false, false, false, false])
  for (const f of folds.slice(0, 5)) expect(within(f).getByRole('button', { name: 'Save', hidden: true })).toBeInTheDocument()
  expect(within(folds[5]).getAllByRole('radiogroup', { hidden: true })).toHaveLength(2)
  expect(screen.queryByRole('navigation')).toBeNull()
  expect(screen.queryByRole('heading', { level: 2 })).toBeNull()
})

test('choosing a palette and a theme sets the root and the stored keys', async () => {
  render(<Settings />)
  const user = userEvent.setup()
  const [palette, theme] = screen.getAllByRole('radiogroup')
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
