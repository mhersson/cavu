import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import App from '../../App.tsx'
import type { Task, TaskEvent } from '../../api/types.ts'
import { clock } from '../../components/time.ts'
import { FakeEventSource, fakeServer, json, narrow, type Call } from '../../test-server.ts'

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
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

const draft: Task = {
  ...base,
  status: 'draft',
  dependencies: [
    { id: 3, status: 'done' },
    { id: 5, status: 'running' },
  ],
}

const isTask = (c: Call) => c.method === 'GET' && c.path === '/api/tasks/7'
const reads = (calls: Call[]) => calls.filter(isTask)
const patches = (calls: Call[]) => calls.filter((c) => c.method === 'PATCH')

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
const button = (name: string) => screen.queryByRole('button', { name })
const headButtons = () => within(document.querySelector<HTMLElement>('.page-head')!).queryAllByRole('button').map((b) => b.textContent)
const field = (name: string) => screen.queryByRole('textbox', { name })
const type = (name: string, value: string) => fireEvent.change(field(name)!, { target: { value } })
const fields = () => ['Title', 'Details', 'Depends on'].map(field)

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

test('a draft offers Edit and Start, and a started task neither', async () => {
  serve(draft)
  await show()
  expect(headButtons()).toEqual(['Edit', 'Start', 'Cancel task'])
  cleanup()

  serve(base)
  await show()
  expect(headButtons()).toEqual(['Cancel task'])
})

test.each([
  ['the title', () => type('Title', 'Fix the header'), { title: 'Fix the header' }],
  ['the dependencies', () => type('Depends on', '3, 42'), { depends_on: [3, 42] }],
  ['the body', () => type('Details', ''), { body: '' }],
])('Edit sends only what changed and shows the task it gets back: %s', async (_, change, sent) => {
  const answer = { ...draft, title: 'Fix the footer on every page' }
  const calls = serve(draft, (c) => (c.method === 'PATCH' ? json(answer) : undefined))
  await show()
  fireEvent.click(button('Edit')!)
  change()
  fireEvent.click(button('Save')!)
  await settle()
  const sends = patches(calls)
  expect(sends).toHaveLength(1)
  expect(sends[0].path).toBe('/api/tasks/7')
  expect(sends[0].body).toEqual(sent)
  expect(sends[0].headers.get('X-CAVU-CSRF')).toBe('1')
  expect(screen.getByRole('heading', { level: 1, name: answer.title })).toBeInTheDocument()
  expect(fields()).toEqual([null, null, null])
})

test('Save with nothing changed and Cancel send nothing and end the edit', async () => {
  const calls = serve(draft)
  await show()
  fireEvent.click(button('Edit')!)
  expect(headButtons()).toEqual(['Cancel', 'Save'])
  expect(field('Title')).toHaveValue(draft.title)
  expect(field('Details')).toHaveValue(draft.body)
  expect(field('Depends on')).toHaveValue('3, 5')
  fireEvent.click(button('Save')!)
  await settle()
  expect(fields()).toEqual([null, null, null])
  expect(headButtons()).toEqual(['Edit', 'Start', 'Cancel task'])

  fireEvent.click(button('Edit')!)
  type('Title', 'Fix the header')
  type('Details', '')
  fireEvent.click(button('Cancel')!)
  expect(fields()).toEqual([null, null, null])
  expect(screen.getByRole('heading', { level: 1, name: draft.title })).toBeInTheDocument()
  expect(document.querySelector('.task-body')).toHaveTextContent('cut off.')
  expect(calls.filter((c) => c.method !== 'GET')).toHaveLength(0)
})

test('a draft with no body shows the Details box when Edit is pressed', async () => {
  serve({ ...draft, body: '' })
  await show()
  fireEvent.click(button('Edit')!)
  expect(field('Details')).toHaveValue('')
})

test('the Details box opens twelve rows high on a desktop and eight on a phone, and taller for a longer body', async () => {
  const rows = async (t: Task) => {
    serve(t)
    await show()
    fireEvent.click(button('Edit')!)
    const n = Number(field('Details')!.getAttribute('rows'))
    cleanup()
    return n
  }
  const long = { ...draft, body: Array.from({ length: 20 }, (_, i) => `line ${i + 1}`).join('\n') }
  expect(await rows(draft)).toBe(12)
  expect(await rows(long)).toBeGreaterThan(20)
  narrow(true)
  expect(await rows(draft)).toBe(8)
  expect(await rows(long)).toBeGreaterThan(20)
})

test("a refused edit shows the server's line and keeps what was typed", async () => {
  const line = 'task 42 does not exist'
  const calls = serve(draft, (c) => (c.method === 'PATCH' ? json({ error: line }, 400) : undefined))
  await show()
  fireEvent.click(button('Edit')!)
  type('Title', 'Fix the header')
  type('Depends on', '42')
  fireEvent.click(button('Save')!)
  await settle()
  expect(patches(calls)).toHaveLength(1)
  expect(screen.getByRole('alert')).toHaveTextContent(line)
  expect(field('Title')).toHaveValue('Fix the header')
  expect(field('Depends on')).toHaveValue('42')
  expect(button('Save')).toBeEnabled()
})

test('a read while editing keeps what was typed, and Save measures against the task Edit was pressed on', async () => {
  let now = draft
  const calls = serve(draft, (c) => (isTask(c) || c.method === 'PATCH' ? json(now) : undefined))
  await show()
  fireEvent.click(button('Edit')!)
  type('Details', 'The footer is cut off on a phone.')
  now = { ...draft, title: 'Fix the footer and the header' }
  const before = reads(calls).length
  act(() => FakeEventSource.all[0].send(1, { task_id: 7 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  await settle()
  expect(reads(calls)).toHaveLength(before + 1)
  expect(field('Title')).toHaveValue(draft.title)
  expect(field('Details')).toHaveValue('The footer is cut off on a phone.')

  fireEvent.click(button('Save')!)
  await settle()
  expect(patches(calls).map((c) => c.body)).toEqual([{ body: 'The footer is cut off on a phone.' }])
})

test('Start sends start and shows the task it gets back', async () => {
  const calls = serve(draft, (c) => (c.method === 'POST' ? json({ ...draft, status: 'queued' }) : undefined))
  await show()
  fireEvent.click(button('Start')!)
  await settle()
  const posts = calls.filter((c) => c.method === 'POST')
  expect(posts).toHaveLength(1)
  expect(posts[0].path).toBe('/api/tasks/7/actions')
  expect(posts[0].body).toEqual({ action: 'start' })
  expect(posts[0].headers.get('X-CAVU-CSRF')).toBe('1')
  expect(headButtons()).toEqual(['Cancel task'])
})

test('on a phone a draft offers Start, Edit and Cancel task at the foot, and an edit Save and Cancel', async () => {
  narrow(true)
  serve(draft)
  await show()
  const offered = () =>
    screen
      .getAllByRole('button')
      .map((b) => b.textContent)
      .filter((n) => ['Start', 'Edit', 'Cancel task', 'Save', 'Cancel'].includes(n ?? ''))
  expect(offered()).toEqual(['Start', 'Edit', 'Cancel task'])
  expect(headButtons()).toEqual([])

  fireEvent.click(button('Edit')!)
  expect(offered()).toEqual(['Save', 'Cancel'])
  expect(headButtons()).toEqual([])
  expect(fields().every((f) => f !== null)).toBe(true)
})

test.each([
  ['start', draft, () => fireEvent.click(button('Start')!)],
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

// jsdom lays nothing out: here a paragraph takes a line per 60 characters, and the fold shows six lines.
function layOut() {
  const line = 20
  const height = (el: Element) =>
    line * (el.textContent ?? '').split('\n').reduce((n, p) => n + Math.max(1, Math.ceil(p.length / 60)), 0)
  vi.spyOn(Element.prototype, 'scrollHeight', 'get').mockImplementation(function (this: Element) {
    return height(this)
  })
  vi.spyOn(Element.prototype, 'clientHeight', 'get').mockImplementation(function (this: Element) {
    return this.classList.contains('fold') ? Math.min(height(this), 6 * line) : height(this)
  })
}

const longBody = Array.from({ length: 12 }, (_, i) => `Step ${i + 1} of what done looks like.`).join('\n')
const body = () => document.querySelector<HTMLElement>('.task-body p')!
const hidden = () => body().scrollHeight - body().clientHeight
const toggle = () => screen.queryAllByRole('button').find((b) => b.getAttribute('aria-controls') === body().id) ?? null

test.each([
  ['a desktop', false],
  ['a phone', true],
])('on %s a started task with a long body shows its first lines and a button that shows it whole and folds it again', async (_, phone) => {
  layOut()
  narrow(phone)
  serve({ ...base, body: longBody })
  await show()
  expect(body()).toHaveTextContent('Step 12')
  expect(hidden()).toBeGreaterThan(0)
  expect(toggle()).toHaveAttribute('aria-expanded', 'false')

  toggle()!.focus()
  expect(toggle()).toHaveFocus()
  fireEvent.click(toggle()!)
  expect(hidden()).toBe(0)
  expect(toggle()).toHaveAttribute('aria-expanded', 'true')

  fireEvent.click(toggle()!)
  expect(hidden()).toBeGreaterThan(0)
  expect(toggle()).toHaveAttribute('aria-expanded', 'false')
})

test('a started task folds one long paragraph by the lines it wraps to', async () => {
  layOut()
  serve({ ...base, body: 'The footer is cut off on a phone. '.repeat(20) })
  await show()
  expect(hidden()).toBeGreaterThan(0)
  expect(toggle()).toHaveAttribute('aria-expanded', 'false')
})

test.each([
  ['a started task with a short body', base],
  ['a draft with a long body', { ...draft, body: longBody }],
])('%s shows its whole body and no button for it', async (_, t) => {
  layOut()
  serve(t)
  await show()
  expect(body()).toHaveTextContent(t.body.split('\n').at(-1)!)
  expect(hidden()).toBe(0)
  expect(toggle()).toBeNull()
  expect(screen.queryAllByRole('button').filter((b) => b.hasAttribute('aria-expanded'))).toEqual([])
})

test('a body shown whole stays whole while the task changes under the page, and a new visit starts folded', async () => {
  layOut()
  let now: Task = { ...base, body: longBody }
  const calls = serve(now, (c) => (isTask(c) ? json(now) : undefined))
  await show()
  fireEvent.click(toggle()!)
  now = { ...now, status: 'in_review', turns: 30 }
  const before = reads(calls).length
  act(() => FakeEventSource.all[0].send(1, { task_id: 7 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  await settle()
  expect(reads(calls)).toHaveLength(before + 1)
  expect(hidden()).toBe(0)
  expect(toggle()).toHaveAttribute('aria-expanded', 'true')

  cleanup()
  await show()
  expect(hidden()).toBeGreaterThan(0)
  expect(toggle()).toHaveAttribute('aria-expanded', 'false')
})

test('a draft that starts folds its long body', async () => {
  layOut()
  const long = { ...draft, body: longBody }
  serve(long, (c) => (c.method === 'POST' ? json({ ...long, status: 'queued' }) : undefined))
  await show()
  expect(toggle()).toBeNull()
  fireEvent.click(button('Start')!)
  await settle()
  expect(hidden()).toBeGreaterThan(0)
  expect(toggle()).toHaveAttribute('aria-expanded', 'false')
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
  serve(base)
  await show()
  expect(document.querySelector('p.small')).toBeNull()
  cleanup()

  serve({ ...base, cost_missing_sessions: 1 })
  await show()
  expect(document.querySelector('p.small')).toHaveTextContent('1')
})

const ev = (id: number, type: string, actor: TaskEvent['actor'] = 'engine'): TaskEvent => ({
  id,
  task_id: 7,
  run_id: 1,
  at: '2030-01-02T03:04:05Z',
  type,
  actor,
  payload: {},
  text: `${type} ${id}`,
})

const eventsQuery = (c: Call) => {
  const u = new URL(c.path, 'http://cavu.test')
  return u.pathname === '/api/tasks/7/events' ? Object.fromEntries(u.searchParams) : null
}
// The query of each request for the task's events, in order.
const eventReads = (calls: Call[]) => calls.map(eventsQuery).filter((q) => q !== null)

// answer gives the events for a request's query.
function serveEvents(answer: (q: Record<string, string>) => TaskEvent[]) {
  return serve(base, (c) => {
    const q = eventsQuery(c)
    return q ? json({ events: answer(q) }) : undefined
  })
}

const foldOf = () => document.querySelector<HTMLDetailsElement>('.tl > details')!

test("the page asks for events without the agent's steps first", async () => {
  const full = Array.from({ length: 1000 }, (_, i) => ev(i + 1, 'pr_observed', 'runner'))
  const calls = serveEvents((q) => (q.after === undefined ? full : q.after === '1000' ? [ev(1001, 'pr_observed', 'runner')] : []))
  await show()
  expect(eventReads(calls)).toEqual([
    { agent: 'false', limit: '1000' },
    { agent: 'false', limit: '1000', after: '1000' },
  ])
  expect(screen.getByText('pr_observed 1001')).toBeInTheDocument()
})

test("opening a fold reads the agent's steps of that stretch", async () => {
  const opening = [ev(1, 'run_started'), ev(10, 'session_started'), ev(500, 'session_ended', 'runner'), ev(501, 'verify_started')]
  // every event after the start: the agent's steps, the session's end, and more past it
  const after = Array.from({ length: 600 }, (_, i) => i + 11).map((id) => opening.find((e) => e.id === id) ?? ev(id, 'agent_tool', 'agent'))
  const calls = serveEvents((q) => (q.agent === 'false' ? opening : after.filter((e) => e.id > Number(q.after)).slice(0, 200)))
  await show()
  expect(eventReads(calls)).toHaveLength(1)
  expect(foldOf().open).toBe(false)

  const toggle = () => fireEvent.click(foldOf().querySelector('summary')!)
  toggle()
  await settle()
  expect(eventReads(calls).slice(1)).toEqual([{ after: '10' }, { after: '210' }, { after: '410' }])
  expect(foldOf().querySelectorAll('.ev')).toHaveLength(489)
  expect(foldOf().querySelector('summary')).toHaveTextContent(/^489 /)
  expect(screen.queryByText('agent_tool 502')).toBeNull()

  toggle()
  toggle()
  await settle()
  expect(foldOf().open).toBe(true)
  expect(eventReads(calls)).toHaveLength(4)
})

test("the live session's steps are read with the page", async () => {
  const calls = serveEvents((q) =>
    q.agent === 'false' ? [ev(1, 'run_started'), ev(5, 'session_started')] : q.after === '5' ? [ev(6, 'agent_tool', 'agent'), ev(7, 'agent_text', 'agent')] : [],
  )
  await show()
  expect(eventReads(calls)).toEqual([{ agent: 'false', limit: '1000' }, { after: '5' }])
  expect(foldOf().open).toBe(true)
  expect(within(foldOf()).getByText('agent_text 7')).toBeInTheDocument()
})

test('a new event appears without a reload', async () => {
  const calls = serveEvents((q) => (q.agent === 'false' ? [ev(1, 'run_started'), ev(2, 'prepared', 'runner')] : q.after === '2' ? [ev(4, 'verify_started')] : []))
  await show()
  act(() => FakeEventSource.all[0].send(3, { task_id: 8 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  await settle()
  expect(eventReads(calls)).toHaveLength(1)

  const before = reads(calls).length
  act(() => FakeEventSource.all[0].send(4, { task_id: 7 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  await settle()
  expect(reads(calls)).toHaveLength(before + 1)
  expect(eventReads(calls).slice(1)).toEqual([{ after: '2', limit: '1000' }])
  expect(screen.getByText('verify_started 4')).toBeInTheDocument()
})

test('a reconnect reads the events after the newest', async () => {
  const calls = serveEvents((q) => (q.agent === 'false' ? [ev(1, 'run_started'), ev(2, 'prepared', 'runner')] : q.after === '2' ? [ev(3, 'verify_started')] : []))
  await show()
  const es = FakeEventSource.all[0]
  act(() => es.open())
  act(() => es.open())
  await settle()
  expect(eventReads(calls)).toEqual([{ agent: 'false', limit: '1000' }, { after: '2', limit: '1000' }])
  expect(screen.getByText('verify_started 3')).toBeInTheDocument()
})
