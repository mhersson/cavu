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

const column = (name: string) => screen.getByRole('region', { name })
const titles = (name: string) => within(column(name)).queryAllByRole('link').map((l) => within(l).getByText(/^Task \d+$/).textContent)

test('each task is in the column of its status as served', async () => {
  serve({
    tasks: [row(1, 'running'), row(2, 'draft'), row(3, 'running'), row(4, 'in_review'), row(5, 'blocked'), row(6, 'queued')],
  })
  await show()
  expect(titles('draft')).toEqual(['Task 2'])
  expect(titles('queued')).toEqual(['Task 6'])
  expect(titles('running')).toEqual(['Task 1', 'Task 3'])
  expect(titles('blocked')).toEqual(['Task 5'])
  expect(titles('in_review')).toEqual(['Task 4'])
  expect(within(column('running')).getByRole('link', { name: /Task 3/ })).toHaveAttribute('href', '/tasks/3')
  expect(within(column('running')).getByRole('heading')).toHaveTextContent('2')
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

test('a task that needs you carries the mark', async () => {
  serve({ tasks: [row(1, 'blocked', { needs_you: true }), row(2, 'blocked')] })
  await show()
  const [needing, other] = within(column('blocked')).getAllByRole('link')
  expect(within(needing).getByText('needs you')).toBeInTheDocument()
  expect(within(other).queryByText('needs you')).toBeNull()
})

test('an event moves a task to its new column without a reload', async () => {
  const world = { tasks: [row(1, 'queued')] }
  const calls = serve(world)
  await show()
  expect(titles('queued')).toEqual(['Task 1'])
  world.tasks = [row(1, 'running')]
  act(() => FakeEventSource.all[0].send(1, { task_id: 1 }))
  await act(() => vi.advanceTimersByTimeAsync(2000))
  expect(titles('queued')).toEqual([])
  expect(titles('running')).toEqual(['Task 1'])
  expect(listReads(calls)).toHaveLength(2)
})

test('a column with nothing says so', async () => {
  serve({ tasks: [row(1, 'running')] })
  await show()
  expect(within(column('running')).queryByText('None')).toBeNull()
  expect(within(column('draft')).getByText('None')).toBeInTheDocument()
})

test('a wait is shown with the time it lasts until', async () => {
  const until = '2030-01-02T14:05:00Z'
  serve({ tasks: [row(1, 'blocked', { waiting: 'the wait as served', waiting_until: until })] })
  await show()
  const item = within(column('blocked')).getByRole('link')
  expect(item).toHaveTextContent('the wait as served')
  expect(item).toHaveTextContent(clock(until))
})
