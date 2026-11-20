import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import App from '../../App.tsx'
import type { Task } from '../../api/types.ts'
import { clock } from '../../components/time.ts'
import { FakeEventSource, fakeServer, json, narrow, type Call } from '../../test-server.ts'

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const base: Task = {
  id: 7,
  project: 'web',
  title: 'Fix the footer',
  body: 'The footer is\ncut off.',
  requester: 'api',
  created_at: '2030-01-02T03:04:00Z',
  status: 'running',
  needs_you: false,
  needs: '',
  needs_you_since: null,
  waiting: '',
  waiting_until: null,
  dependencies: [],
  stop: null,
  questions: [],
  review: null,
  pull_request: null,
  change: null,
  turns: 12,
  duration_ms: 226000,
  cost_estimate_usd: 0.325,
  cost_missing_sessions: 0,
  usage: [],
  budget: { turns: 12, max_turns: 100, minutes: 4, max_minutes: 300 },
}

const pull = {
  number: 21,
  url: 'https://example.test/pull/21',
  branch: 'cavu/7',
  head: '0123456789abcdef',
  state: 'open' as const,
  checks: 'passed' as const,
  failed_checks: [],
  ready: true,
}

const change = {
  head_commit: 'fedcba9876543210',
  files_changed: 2,
  diff_stat: [' a.go | 2 ++', ' 1 file changed, 2 insertions(+)'],
  flagged: { workflows: [], instructions: [], large: [] },
}

const stoppedForRetry: Task = {
  ...base,
  status: 'blocked',
  needs_you: true,
  needs: 'retry',
  needs_you_since: '2030-01-02T04:00:00Z',
  stop: { reason: 'ci_failed', explanation: 'x', action: 'retry', resets_at: null },
}

const toTake: Task = {
  ...base,
  status: 'in_review',
  needs_you: true,
  needs: 'take',
  needs_you_since: '2030-01-02T04:00:00Z',
  change,
}

const isTask = (c: Call) => c.method === 'GET' && c.path === '/api/tasks/7'
const reads = (calls: Call[]) => calls.filter(isTask)

function serve(t: Task, more: (c: Call) => Response | undefined = () => undefined) {
  return fakeServer((c) => {
    const answer = more(c)
    if (answer) return answer
    if (isTask(c)) return json(t)
    if (c.path === '/api/tasks?needs_you=true') return json({ tasks: [] })
  })
}

async function settle() {
  for (let i = 0; i < 3; i++) await act(() => vi.advanceTimersByTimeAsync(0))
}

async function show() {
  vi.useFakeTimers()
  render(
    <MemoryRouter initialEntries={['/tasks/7']}>
      <App />
    </MemoryRouter>,
  )
  await settle()
}

const cancelButton = () => screen.queryByRole('button', { name: 'Cancel task' })

test('the wait and its end are shown as served', async () => {
  const waiting = 'Waiting for the usage limit to reset'
  serve({ ...base, waiting, waiting_until: '2030-01-02T09:30:00Z' })
  await show()
  const line = screen.getByText(waiting, { exact: false })
  expect(line).toHaveTextContent(clock('2030-01-02T09:30:00Z'))
  expect(screen.queryByRole('article')).toBeNull()
})

test('cancel asks first and sends cancel', async () => {
  const calls = serve(base, (c) => (c.method === 'POST' ? json({ ...base, status: 'cancelled' }) : undefined))
  await show()
  fireEvent.click(cancelButton()!)
  const dialog = screen.getByRole('dialog')
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)

  fireEvent.click(within(dialog).getAllByRole('button')[1])
  await settle()
  const posts = calls.filter((c) => c.method === 'POST')
  expect(posts).toHaveLength(1)
  expect(posts[0].path).toBe('/api/tasks/7/actions')
  expect(posts[0].body).toEqual({ action: 'cancel' })
  expect(posts[0].headers.get('X-CAVU-CSRF')).toBe('1')
  expect(screen.queryByRole('dialog')).toBeNull()
  // the answer is the page's task: a cancelled task offers no cancel
  expect(cancelButton()).toBeNull()
})

test('the dialog closes with nothing sent by its other button and by Escape', async () => {
  const calls = serve(base)
  await show()
  fireEvent.click(cancelButton()!)
  fireEvent.click(within(screen.getByRole('dialog')).getAllByRole('button')[0])
  expect(screen.queryByRole('dialog')).toBeNull()
  fireEvent.click(cancelButton()!)
  fireEvent.keyDown(document, { key: 'Escape' })
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
})

test.each([
  ['retry', stoppedForRetry, () => fireEvent.click(screen.getByRole('button', { name: 'Retry' }))],
  ['cancel', base, () => {
    fireEvent.click(cancelButton()!)
    fireEvent.click(within(screen.getByRole('dialog')).getAllByRole('button')[1])
  }],
  ['the recorded merge', toTake, () => fireEvent.click(screen.getByRole('button', { name: 'Record the merge' }))],
])('a refused action shows the server\'s line and reads the task again: %s', async (_, t, act1) => {
  const line = 'that was decided a moment ago'
  const calls = serve(t, (c) => (c.method === 'POST' ? json({ error: line }, 400) : undefined))
  await show()
  const before = reads(calls).length
  act1()
  await settle()
  expect(screen.getByRole('alert')).toHaveTextContent(line)
  expect(screen.queryByRole('dialog')).toBeNull()
  const post = calls.findIndex((c) => c.method === 'POST')
  expect(calls.slice(post + 1).some(isTask)).toBe(true)
  expect(reads(calls).length).toBeGreaterThan(before)
})

test('a task with no pull request shows no pull request section', async () => {
  serve(base)
  await show()
  expect(screen.queryByRole('heading', { level: 3, name: 'Pull request' })).toBeNull()
})

test('a task with a pull request shows its section', async () => {
  serve({ ...base, pull_request: pull })
  await show()
  expect(screen.getByRole('heading', { level: 3, name: 'Pull request' })).toBeInTheDocument()
})

test('an event about the task reads it again, and one about another task does not', async () => {
  const calls = serve(base)
  await show()
  const first = reads(calls).length
  act(() => FakeEventSource.all[0].send(1, { task_id: 8 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  await settle()
  expect(reads(calls)).toHaveLength(first)

  act(() => FakeEventSource.all[0].send(2, { task_id: 7 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  await settle()
  expect(reads(calls)).toHaveLength(first + 1)
  expect(FakeEventSource.all).toHaveLength(1)
})

test('a reconnect reads the task again', async () => {
  const calls = serve(base)
  await show()
  const first = reads(calls).length
  const es = FakeEventSource.all[0]
  act(() => es.open())
  act(() => es.open())
  await settle()
  expect(reads(calls)).toHaveLength(first + 1)
})

test('a task the server does not find shows the server\'s line', async () => {
  const line = 'no such task'
  serve(base, (c) => (isTask(c) ? json({ error: line }, 404) : undefined))
  await show()
  expect(screen.getByText(line)).toBeInTheDocument()
  expect(screen.queryByRole('heading', { level: 1 })).toBeNull()
})

test('the money meters show only when served', async () => {
  serve(base)
  await show()
  expect(screen.getAllByRole('meter')).toHaveLength(2)
  cleanup()

  serve({ ...base, budget: { ...base.budget, usd: 1, max_usd: 5, day_usd: 2, max_day_usd: 20 } })
  await show()
  const meters = screen.getAllByRole('meter')
  expect(meters).toHaveLength(4)
  expect(meters.map((m) => m.getAttribute('aria-valuenow'))).toEqual(['12', '4', '1', '2'])
  expect(meters.map((m) => m.getAttribute('aria-valuemax'))).toEqual(['100', '300', '5', '20'])
})

test('a meter past its cap stays full', async () => {
  serve({ ...base, budget: { ...base.budget, turns: 150 } })
  await show()
  const fill = screen.getAllByRole('meter')[0].firstElementChild as HTMLElement
  expect(fill.style.width).toBe('100%')
})

test.each(['done', 'cancelled'] as const)('a %s task offers no cancel', async (status) => {
  serve({ ...base, status })
  await show()
  expect(cancelButton()).toBeNull()
})

test('a task that is running offers cancel', async () => {
  serve(base)
  await show()
  expect(cancelButton()).not.toBeNull()
})

test('the usage table has a row per model and a dependency links to its page', async () => {
  serve({
    ...base,
    usage: [{ source: 'proxy', model: 'claude-opus-5', input: 1930000, output: 61000, cache_read: 5, cache_write: 812, cost_estimate_usd: 1 }],
    dependencies: [{ id: 3, status: 'done' }],
  })
  await show()
  const rows = screen.getAllByRole('row')
  expect(rows).toHaveLength(2)
  expect(within(rows[1]).getAllByRole('cell').map((c) => c.textContent)).toEqual(['opus-5', '1.93 M', '61 k', '5', '812'])
  expect(screen.getByRole('link', { name: /3/ })).toHaveAttribute('href', '/tasks/3')
})

test('on a phone the rail\'s sections fold and only those with something show', async () => {
  narrow(true)
  serve({ ...base, pull_request: pull })
  await show()
  const folds = [...document.querySelectorAll('details.acc')]
  expect(folds.map((d) => d.querySelector('summary')!.textContent)).toEqual(['Pull request', 'Cost and use'])
  expect(screen.queryByRole('heading', { level: 3, name: 'Change' })).toBeNull()
  expect(cancelButton()).not.toBeNull()
  expect(screen.getAllByRole('button', { name: 'Cancel task' })).toHaveLength(1)
})

test('the sessions without a cost are counted only when there are some', async () => {
  serve({ ...base, cost_missing_sessions: 1 })
  await show()
  expect(document.querySelector('p.small')).toHaveTextContent('1')
})
