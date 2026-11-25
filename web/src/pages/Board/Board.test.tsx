import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import App from '../../App.tsx'
import type { Row, Status } from '../../api/types.ts'
import { clock } from '../../components/time.ts'
import { FakeEventSource, fakeServer, json, narrow, type Call } from '../../test-server.ts'

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

function row(id: number, status: Status, more: Partial<Row> = {}): Row {
  return {
    id,
    project: 'web',
    title: `Task ${id}`,
    status,
    needs_you: false,
    needs: '',
    needs_you_since: null,
    stop_reason: '',
    waiting: '',
    waiting_until: null,
    pull_request_ready: false,
    created_at: '2030-01-02T03:00:00Z',
    ...more,
  }
}

const open = '/api/tasks?status=draft,queued,running,blocked,in_review'
const listReads = (calls: Call[]) => calls.filter((c) => c.method === 'GET' && c.path.startsWith('/api/tasks?status='))

// The server answers from world, which a test changes between reads: a task is listed under the statuses a request names.
function serve(world: { tasks: Row[] }) {
  return fakeServer((c) => {
    if (c.path === '/api/projects') return json({ projects: [{ id: 1, name: 'web' }, { id: 2, name: 'api' }] })
    if (c.path === '/api/tasks?needs_you=true') return json({ tasks: [] }) // the frame's count
    if (!c.path.startsWith('/api/tasks?')) return undefined
    const q = new URL(c.path, 'http://x').searchParams
    const statuses = q.get('status')!.split(',')
    const project = q.get('project')
    return json({
      tasks: world.tasks.filter((r) => statuses.includes(r.status) && (!project || r.project === project)),
    })
  })
}

async function show(path = '/board') {
  vi.useFakeTimers()
  narrow(false)
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  )
  await act(() => vi.advanceTimersByTimeAsync(0))
}

const group = (name: string) => screen.getByRole('region', { name })
const titles = (name: string) => within(group(name)).queryAllByRole('link').map((l) => within(l).getByText(/^Task \d+$/).textContent)

test('each task is in the group of its status as served', async () => {
  serve({
    tasks: [row(1, 'running'), row(2, 'draft'), row(3, 'running'), row(4, 'in_review'), row(5, 'blocked'), row(6, 'queued')],
  })
  await show()
  expect(screen.getAllByRole('region').map((el) => el.getAttribute('aria-label'))).toEqual(['running', 'queued', 'blocked', 'in_review', 'draft'])
  expect(titles('draft')).toEqual(['Task 2'])
  expect(titles('queued')).toEqual(['Task 6'])
  expect(titles('running')).toEqual(['Task 1', 'Task 3'])
  expect(titles('blocked')).toEqual(['Task 5'])
  expect(titles('in_review')).toEqual(['Task 4'])
  expect(within(group('running')).getByRole('link', { name: /Task 3/ })).toHaveAttribute('href', '/tasks/3')
  expect(within(group('running')).getByRole('heading')).toHaveTextContent('2')
})

test('the board asks for finished tasks only when ticked', async () => {
  const calls = serve({ tasks: [row(1, 'running'), row(2, 'done'), row(3, 'cancelled')] })
  await show()
  expect(listReads(calls).map((c) => c.path)).toEqual([open])
  expect(screen.queryByRole('region', { name: 'done' })).toBeNull()

  fireEvent.click(screen.getByRole('checkbox'))
  await act(() => vi.advanceTimersByTimeAsync(0))
  expect(listReads(calls).slice(1).map((c) => c.path)).toEqual([
    open,
    '/api/tasks?status=done&order=newest&limit=30',
    '/api/tasks?status=cancelled&order=newest&limit=30',
  ])
  expect(titles('done')).toEqual(['Task 2'])
  expect(titles('cancelled')).toEqual(['Task 3'])
})

test('a ticked address asks for finished tasks from the start', async () => {
  const calls = serve({ tasks: [] })
  await show('/board?finished=1')
  expect(listReads(calls)).toHaveLength(3)
  expect(screen.getByRole('checkbox')).toBeChecked()
})

test('the project filter asks with the project', async () => {
  const calls = serve({ tasks: [row(1, 'running'), row(2, 'running', { project: 'api' })] })
  await show()
  fireEvent.change(screen.getByRole('combobox', { name: 'Project' }), { target: { value: 'api' } })
  await act(() => vi.advanceTimersByTimeAsync(0))
  expect(listReads(calls).map((c) => c.path)).toEqual([open, open + '&project=api'])
  expect(titles('running')).toEqual(['Task 2'])
})

test('each task links to its requested human action and a task working on its own has none', async () => {
  serve({ tasks: [
    row(1, 'blocked', { needs_you: true, needs: 'answer' }),
    row(2, 'blocked', { needs_you: true, needs: 'retry', stop_reason: 'ci_failed' }),
    row(3, 'in_review', { needs_you: true, needs: 'review' }),
    row(4, 'in_review', { needs_you: true, needs: 'take' }),
    row(5, 'running'),
  ] })
  await show()
  for (const [name, id] of [[/answer/i, 1], [/retry.*CI/i, 2], [/review/i, 3], [/take/i, 4]] as const) {
    expect(screen.getByRole('link', { name })).toHaveAttribute('href', '/tasks/' + id)
  }
  expect(within(group('running')).getByRole('link')).not.toHaveAccessibleName(/answer|retry|review|take/i)
})

test('an event moves a task to its new group without a reload', async () => {
  const world = { tasks: [row(1, 'queued')] }
  const calls = serve(world)
  await show()
  expect(titles('queued')).toEqual(['Task 1'])
  world.tasks = [row(1, 'running')]
  act(() => FakeEventSource.all[0].send(1, { task_id: 1 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  expect(screen.queryByRole('region', { name: 'queued' })).toBeNull()
  expect(titles('running')).toEqual(['Task 1'])
  expect(listReads(calls)).toHaveLength(2)
})

test('a status can be collapsed and a search reveals matching tasks', async () => {
  const calls = serve({ tasks: [row(1, 'running'), row(12, 'running', { title: 'Fix the parser for #123' })] })
  await show()
  expect(screen.queryAllByRole('region')).toEqual([group('running')])
  fireEvent.click(within(group('running')).getByRole('button'))
  expect(within(group('running')).queryAllByRole('link')).toHaveLength(0)
  fireEvent.click(within(group('running')).getByRole('button', { expanded: false }))
  expect(within(group('running')).getAllByRole('link')).toHaveLength(2)
  fireEvent.click(within(group('running')).getByRole('button'))

  const search = screen.getByRole('searchbox')
  for (const value of [' PARSER ', '#12', '12']) {
    fireEvent.change(search, { target: { value } })
    expect(within(group('running')).getByRole('link')).toHaveAttribute('href', '/tasks/12')
  }
  fireEvent.change(search, { target: { value: '#1' } })
  expect(within(group('running')).getByRole('link')).toHaveAttribute('href', '/tasks/1')
  fireEvent.change(search, { target: { value: '#2' } })
  expect(screen.queryAllByRole('region')).toHaveLength(0)
  fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }))
  expect(within(group('running')).getAllByRole('link')).toHaveLength(2)
  expect(listReads(calls).map((c) => c.path)).toEqual([open])
})

test('a wait is shown with the time it lasts until', async () => {
  const until = '2030-01-02T14:05:00Z'
  serve({ tasks: [row(1, 'blocked', { waiting: 'the wait as served', waiting_until: until })] })
  await show()
  const item = within(group('blocked')).getByRole('link')
  expect(item).toHaveTextContent('the wait as served')
  expect(item).toHaveTextContent(clock(until))
})

test('a full finished group says it shows the newest', async () => {
  const done = Array.from({ length: 30 }, (_, i) => row(i + 1, 'done'))
  serve({ tasks: [...done, row(31, 'cancelled'), row(32, 'cancelled')] })
  await show('/board?finished=1')
  expect(within(group('done')).getByRole('heading')).toHaveTextContent('newest 30')
  expect(within(group('cancelled')).getByRole('heading')).not.toHaveTextContent('newest')
  expect(within(group('cancelled')).getByRole('heading')).toHaveTextContent('2')
})

test('typing an ID keeps every character before the next navigation commits', async () => {
  serve({ tasks: [row(1, 'running'), row(31, 'running')] })
  await show()
  const input = screen.getByRole<HTMLInputElement>('searchbox')
  input.focus()
  act(() => {
    for (const char of '#31') fireEvent.change(input, { target: { value: input.value + char } })
  })
  expect(input).toHaveValue('#31')
  expect(within(group('running')).getByRole('link')).toHaveAttribute('href', '/tasks/31')
})

test('overlapping reads show progress and a late older response cannot roll it back', async () => {
  const reads: ((response: Response) => void)[] = []
  fakeServer((c) => {
    if (c.path === open) return new Promise<Response>((resolve) => reads.push(resolve))
    if (c.path === '/api/projects') return json({ projects: [{ id: 1, name: 'web' }] })
    if (c.path === '/api/tasks?needs_you=true') return json({ tasks: [] })
  })
  await show()
  const refresh = async () => {
    act(() => FakeEventSource.all[0].send(1, { task_id: 1 }))
    await act(() => vi.advanceTimersByTimeAsync(2000))
  }
  const finish = (index: number, id: number) => act(async () => {
    reads[index](json({ tasks: [row(id, 'running')] }))
  })
  await refresh()
  await finish(0, 1)
  expect(titles('running')).toEqual(['Task 1'])
  await refresh()
  await finish(2, 3)
  await finish(1, 2)
  expect(titles('running')).toEqual(['Task 3'])
})

test('a saved filter for a removed project can be cleared', async () => {
  fakeServer((c) => {
    if (c.path === '/api/projects') return json({ projects: [{ id: 1, name: 'web' }] })
    if (c.path === open + '&project=removed') return json({ error: 'removed' }, 400)
    if (c.path === open) return json({ tasks: [row(7, 'running')] })
    if (c.path === '/api/tasks?needs_you=true') return json({ tasks: [] })
  })
  await show('/board?project=removed')
  const project = screen.getByRole('combobox', { name: 'Project' })
  expect(project).toHaveValue('removed')
  fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }))
  await act(() => vi.advanceTimersByTimeAsync(0))
  expect(project).toHaveValue('')
  expect(titles('running')).toEqual(['Task 7'])
})

test('search keeps a capped history visible even when every finished task is filtered out', async () => {
  serve({ tasks: Array.from({ length: 30 }, (_, i) => row(i + 10, 'cancelled')) })
  await show('/board?finished=1&q=%231')
  expect(screen.queryAllByRole('region')).toHaveLength(0)
  expect(screen.getByRole('note')).toHaveTextContent('30')
  fireEvent.click(screen.getByRole('checkbox'))
  await act(() => vi.advanceTimersByTimeAsync(0))
  expect(screen.queryByRole('note')).toBeNull()
})

test('search, project and finished edits compose before navigation commits', async () => {
  serve({ tasks: [row(1, 'running'), row(55, 'done'), row(155, 'running', { project: 'api' })] })
  await show('/board?q=%231')
  const input = screen.getByRole<HTMLInputElement>('searchbox')
  const project = screen.getByRole('combobox', { name: 'Project' })
  act(() => {
    fireEvent.change(input, { target: { value: '55' } })
    fireEvent.change(project, { target: { value: 'web' } })
    fireEvent.click(screen.getByRole('checkbox'))
  })
  await act(() => vi.advanceTimersByTimeAsync(0))
  expect(input).toHaveValue('55')
  expect(project).toHaveValue('web')
  expect(screen.getByRole('checkbox')).toBeChecked()
  expect(screen.getAllByRole('region')).toEqual([group('done')])
  expect(titles('done')).toEqual(['Task 55'])
})
