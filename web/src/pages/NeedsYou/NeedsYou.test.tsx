import { act, fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import App from '../../App.tsx'
import type { Row, Task } from '../../api/types.ts'
import { FakeEventSource, fakeServer, json, narrow, type Call } from '../../test-server.ts'

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

function row(id: number, more: Partial<Row> = {}): Row {
  return {
    id,
    project: 'web',
    title: `Task ${id}`,
    status: 'blocked',
    needs_you: true,
    needs: 'answer',
    needs_you_since: '2030-01-02T04:00:00Z',
    stop_reason: 'question',
    waiting: '',
    waiting_until: null,
    pull_request_ready: false,
    created_at: '2030-01-02T03:00:00Z',
    ...more,
  }
}

const stopped = (id: number, since: string) => row(id, { needs: 'retry', stop_reason: 'ci_failed', needs_you_since: since })
const ready = (id: number, since: string) =>
  row(id, { status: 'in_review', needs: 'review', stop_reason: '', pull_request_ready: true, needs_you_since: since })

function task(r: Row): Task {
  return {
    id: r.id,
    project: r.project,
    title: r.title,
    body: '',
    requester: 'api',
    created_at: r.created_at,
    status: r.status,
    needs_you: r.needs_you,
    needs: r.needs,
    needs_you_since: r.needs_you_since,
    waiting: r.waiting,
    waiting_until: r.waiting_until,
    dependencies: [],
    stop:
      r.needs === 'answer' || r.needs === 'retry'
        ? { reason: r.stop_reason || 'question', explanation: 'x', action: r.needs, resets_at: null }
        : null,
    questions: r.needs === 'answer' ? [{ question: 'Which pages?', suggested_answer: '' }] : [],
    review: null,
    pull_request:
      r.needs === 'review'
        ? {
            number: r.id,
            url: `https://example.test/pull/${r.id}`,
            branch: 'b',
            head: '0123456789ab',
            state: 'open',
            checks: 'passed',
            failed_checks: [],
            ready: true,
          }
        : null,
    change: null,
    turns: 0,
    duration_ms: 0,
    cost_estimate_usd: 0,
    cost_missing_sessions: 0,
    usage: [],
    budget: { turns: 0, max_turns: 0, minutes: 0, max_minutes: 0 },
  }
}

const isNeeding = (c: Call) => c.path === '/api/tasks?needs_you=true'
const isWorking = (c: Call) => c.path === '/api/tasks?status=queued,running,blocked,in_review'
const reads = (calls: Call[], id: number) => calls.filter((c) => c.method === 'GET' && c.path === `/api/tasks/${id}`)
const taskReads = (calls: Call[]) => calls.filter((c) => c.method === 'GET' && /^\/api\/tasks\/\d+$/.test(c.path))

type World = { needing: Row[]; working: Row[] }

// The server answers from world, which a test changes between reads; more answers first.
function serve(world: World, more: (c: Call) => Response | undefined = () => undefined) {
  return fakeServer((c) => {
    const answer = more(c)
    if (answer) return answer
    if (isNeeding(c)) return json({ tasks: world.needing })
    if (isWorking(c)) return json({ tasks: world.working })
    const id = Number(c.path.match(/^\/api\/tasks\/(\d+)$/)?.[1])
    const r = world.needing.find((x) => x.id === id)
    if (c.method === 'GET' && r) return json(task(r))
  })
}

async function settle() {
  for (let i = 0; i < 3; i++) await act(() => vi.advanceTimersByTimeAsync(0))
}

async function show() {
  render(
    <MemoryRouter initialEntries={['/']}>
      <App />
    </MemoryRouter>,
  )
  if (vi.isFakeTimers()) await settle()
}

// The list's rows are buttons named by their task's title.
const listed = () => screen.getAllByRole('button', { name: /^Task \d+/ })
const chosen = () => listed().find((b) => b.getAttribute('aria-current') === 'true')
const beside = () => screen.getByRole('heading', { level: 2 })

test('the list is ordered by needs_you_since, the most recent first', async () => {
  vi.useFakeTimers()
  serve({
    needing: [
      row(1, { needs_you_since: '2030-01-02T04:00:00Z' }),
      stopped(2, '2030-01-02T06:00:00Z'),
      // Later than task 1 by half a second, though it sorts before it as text.
      ready(3, '2030-01-02T04:00:00.5Z'),
    ],
    working: [],
  })
  await show()
  expect(listed().map((b) => within(b).getByText(/^Task \d+$/).textContent)).toEqual(['Task 2', 'Task 3', 'Task 1'])
})

test('the first task is the one beside the list', async () => {
  vi.useFakeTimers()
  const calls = serve({ needing: [row(1), stopped(2, '2030-01-02T05:00:00Z')], working: [] })
  await show()
  expect(chosen()).toHaveTextContent('Task 2')
  expect(beside()).toHaveTextContent('Task 2')
  expect(taskReads(calls).map((c) => c.path)).toEqual(['/api/tasks/2'])
})

test('choosing a row reads that task', async () => {
  vi.useFakeTimers()
  const calls = serve({ needing: [row(1), stopped(2, '2030-01-02T05:00:00Z')], working: [] })
  await show()
  fireEvent.click(screen.getByRole('button', { name: /^Task 1/ }))
  await settle()
  expect(reads(calls, 1)).toHaveLength(1)
  expect(chosen()).toHaveTextContent('Task 1')
  expect(beside()).toHaveTextContent('Task 1')
})

test('the chosen task leaving the list chooses the first row', async () => {
  vi.useFakeTimers()
  const world = { needing: [row(1), stopped(2, '2030-01-02T05:00:00Z')], working: [] }
  const calls = serve(world)
  await show()
  fireEvent.click(screen.getByRole('button', { name: /^Task 1/ }))
  await settle()

  world.needing = [stopped(2, '2030-01-02T05:00:00Z'), ready(3, '2030-01-02T06:00:00Z')]
  act(() => FakeEventSource.all[0].send(1, { task_id: 1 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  await settle()
  expect(chosen()).toHaveTextContent('Task 3')
  expect(beside()).toHaveTextContent('Task 3')
  expect(reads(calls, 3)).toHaveLength(1)
})

test('the chosen task holds when a new one arrives at the top', async () => {
  vi.useFakeTimers()
  const world = { needing: [row(1), stopped(2, '2030-01-02T05:00:00Z')], working: [] }
  serve(world)
  await show()
  expect(chosen()).toHaveTextContent('Task 2')

  world.needing = [...world.needing, ready(3, '2030-01-02T06:00:00Z')]
  act(() => FakeEventSource.all[0].send(1, { task_id: 3 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  await settle()
  expect(listed()[0]).toHaveTextContent('Task 3')
  expect(chosen()).toHaveTextContent('Task 2')
  expect(beside()).toHaveTextContent('Task 2')
})

test('after an answer the next row is chosen without waiting for an event', async () => {
  vi.useFakeTimers()
  const world = { needing: [row(1), row(2, { needs_you_since: '2030-01-02T05:00:00Z' })], working: [] }
  serve(world, (c) => {
    if (c.method !== 'POST' || c.path !== '/api/tasks/2/actions') return
    world.needing = [row(1)]
    return json(task(row(2, { status: 'queued', needs_you: false, needs: '', needs_you_since: null, stop_reason: '' })))
  })
  await show()
  expect(chosen()).toHaveTextContent('Task 2')

  fireEvent.change(screen.getByRole('textbox', { name: 'Answer' }), { target: { value: 'Only the changelog.' } })
  fireEvent.click(screen.getByRole('button', { name: 'Answer' }))
  await settle()
  expect(chosen()).toHaveTextContent('Task 1')
  expect(beside()).toHaveTextContent('Task 1')
})

// Task 2 is answered elsewhere a moment before Answer: the action is refused and task 2 needs nothing more.
function answeredElsewhere(world: World, line: string) {
  let gone = false
  return serve(world, (c) => {
    if (c.path === '/api/tasks/2/actions') {
      gone = true
      world.needing = [row(1)]
      return json({ error: line }, 400)
    }
    if (gone && c.path === '/api/tasks/2') {
      return json(task(row(2, { status: 'queued', needs_you: false, needs: '', needs_you_since: null, stop_reason: '' })))
    }
  })
}

async function burst(task_id: number) {
  act(() => FakeEventSource.all[0].send(1, { task_id }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  await settle()
}

test('a refused action whose task needs nothing more stays beside the list until another row is chosen', async () => {
  vi.useFakeTimers()
  const line = 'the question was answered a moment ago'
  answeredElsewhere({ needing: [row(1), row(2, { needs_you_since: '2030-01-02T05:00:00Z' })], working: [] }, line)
  await show()
  fireEvent.click(screen.getByRole('button', { name: 'Answer' }))
  await settle()
  await burst(2)
  expect(listed()).toHaveLength(1)
  expect(beside()).toHaveTextContent('Task 2')
  expect(screen.getByRole('alert')).toHaveTextContent(line)

  fireEvent.click(screen.getByRole('button', { name: /^Task 1/ }))
  await settle()
  expect(beside()).toHaveTextContent('Task 1')
  expect(screen.queryByRole('alert')).toBeNull()
})

test('on a phone a refused action whose task needs nothing more keeps its box', async () => {
  vi.useFakeTimers()
  narrow(true)
  const line = 'the question was answered a moment ago'
  const calls = answeredElsewhere(
    { needing: [row(1), row(2, { needs_you_since: '2030-01-02T05:00:00Z' })], working: [] },
    line,
  )
  await show()
  fireEvent.click(within(screen.getAllByRole('article')[0]).getByRole('button', { name: 'Answer' }))
  await settle()
  const before = reads(calls, 2).length
  await burst(2)
  expect(reads(calls, 2)).toHaveLength(before + 1)
  const boxes = screen.getAllByRole('article')
  expect(boxes).toHaveLength(2)
  expect(within(boxes[0]).getByRole('link', { name: 'Task 2' })).toBeInTheDocument()
  expect(within(boxes[0]).getByRole('alert')).toHaveTextContent(line)
})

test('an event for a shown task reads it again', async () => {
  vi.useFakeTimers()
  const calls = serve({ needing: [row(1), stopped(2, '2030-01-02T05:00:00Z')], working: [] })
  await show()
  const es = FakeEventSource.all[0]
  act(() => es.open())
  expect(reads(calls, 2)).toHaveLength(1)
  expect(calls.filter(isWorking)).toHaveLength(1)

  act(() => es.send(1, { task_id: 2 }))
  act(() => es.send(2, { task_id: 2 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  await settle()
  expect(reads(calls, 2)).toHaveLength(2)
  expect(calls.filter(isWorking)).toHaveLength(2)

  act(() => es.send(3, { task_id: 1 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  await settle()
  expect(reads(calls, 2)).toHaveLength(2)
  expect(reads(calls, 1)).toHaveLength(0)
  expect(calls.filter(isWorking)).toHaveLength(3)
  // The page's and the frame's list of tasks that need a person, after each read.
  expect(calls.filter(isNeeding)).toHaveLength(6)
  expect(FakeEventSource.all).toHaveLength(1)
})

test('nothing needing you shows the empty state and what works without you', async () => {
  vi.useFakeTimers()
  const working = [
    row(5, { status: 'running', needs: '', needs_you: false, needs_you_since: null, stop_reason: '' }),
    row(6, {
      status: 'blocked',
      needs: '',
      needs_you: false,
      needs_you_since: null,
      stop_reason: 'usage_limit',
      waiting: 'Served wait line.',
      waiting_until: '2030-01-02T22:10:00Z',
    }),
    // A task in an open status that needs a person is not working without you.
    stopped(7, '2030-01-02T05:00:00Z'),
  ]
  const calls = serve({ needing: [], working })
  await show()
  expect(calls.filter(isWorking)).toHaveLength(1)
  expect(screen.queryAllByRole('button', { name: /^Task \d+/ })).toHaveLength(0)
  const rows = screen.getAllByRole('row')
  expect(rows).toHaveLength(2)
  expect(within(rows[0]).getByRole('link')).toHaveAttribute('href', '/tasks/5')
  expect(rows[0]).toHaveTextContent('running')
  expect(within(rows[1]).getByRole('link')).toHaveAttribute('href', '/tasks/6')
  expect(rows[1]).toHaveTextContent('blocked')
  expect(rows[1]).toHaveTextContent('Served wait line.')
  expect(taskReads(calls)).toHaveLength(0)
})

test('on a phone each task is read and shown with its action', async () => {
  vi.useFakeTimers()
  narrow(true)
  const calls = serve({
    needing: [row(1), stopped(2, '2030-01-02T05:00:00Z'), ready(3, '2030-01-02T06:00:00Z')],
    working: [],
  })
  await show()
  for (const id of [1, 2, 3]) expect(reads(calls, id)).toHaveLength(1)
  const boxes = screen.getAllByRole('article')
  expect(boxes).toHaveLength(3)
  expect(within(boxes[0]).getByRole('link', { name: 'Open pull request' })).toHaveAttribute(
    'href',
    'https://example.test/pull/3',
  )
  expect(within(boxes[1]).getByRole('button', { name: 'Retry' })).toBeInTheDocument()
  expect(within(boxes[2]).getByRole('button', { name: 'Answer' })).toBeInTheDocument()
})

test('a typed answer survives the sign-in after a 401', async () => {
  let signedIn = true
  serve({ needing: [row(9)], working: [] }, (c) => {
    if (c.path === '/api/auth/login') {
      signedIn = true
      return new Response(null, { status: 204 })
    }
    if (c.method === 'POST' && c.path === '/api/tasks/9/actions') {
      signedIn = false
      return json({ error: 'sign in' }, 401)
    }
    if (!signedIn) return json({ error: 'sign in' }, 401)
  })
  await show()
  const user = userEvent.setup()
  await user.type(await screen.findByRole('textbox', { name: 'Answer' }), 'Only the changelog.')
  await user.click(screen.getByRole('button', { name: 'Answer' }))

  await user.type(await screen.findByLabelText(/password/i), 'pw')
  await user.type(screen.getByRole('textbox'), 'ada')
  await user.click(screen.getByRole('button'))
  expect(await screen.findByRole('textbox', { name: 'Answer' })).toHaveValue('Only the changelog.')
  expect(screen.getByRole('link', { name: /needs you/i })).toHaveAttribute('aria-current', 'page')
})

test('New task opens the dialog', async () => {
  vi.useFakeTimers()
  serve({ needing: [row(1)], working: [] }, (c) => (c.path === '/api/projects' ? json({ projects: [{ runnable: true }] }) : undefined))
  await show()
  expect(screen.queryByRole('dialog')).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'New task' }))
  expect(screen.getByRole('dialog')).toBeInTheDocument()
})
