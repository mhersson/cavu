import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import App from '../../App.tsx'
import type { Row, Status } from '../../api/types.ts'
import { fakeServer, json, narrow, type Call } from '../../test-server.ts'

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
const listReads = (calls: Call[]) => calls.filter((c) => c.method === 'GET' && c.path.startsWith('/api/tasks?status=')).map((c) => c.path)

// A task is listed under the statuses a request names.
function serve(tasks: Row[]) {
  return fakeServer((c) => {
    if (c.path === '/api/tasks?needs_you=true') return json({ tasks: [] }) // the frame's count
    if (!c.path.startsWith('/api/tasks?status=')) return undefined
    const statuses = new URL(c.path, 'http://x').searchParams.get('status')!.split(',')
    return json({ tasks: tasks.filter((r) => statuses.includes(r.status)) })
  })
}

async function show(path = '/status') {
  vi.useFakeTimers()
  narrow(true)
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  )
  await act(() => vi.advanceTimersByTimeAsync(0))
}

const groupNames = () => screen.queryAllByRole('region').map((g) => g.getAttribute('aria-label'))
const group = (name: string) => screen.getByRole('region', { name })

test('Status groups the same tasks on a phone', async () => {
  const calls = serve([
    row(1, 'draft'),
    row(2, 'running'),
    row(3, 'in_review'),
    row(4, 'running', { needs_you: true, waiting: 'Waiting for an answer.' }),
    row(5, 'queued'),
  ])
  await show()

  expect(listReads(calls)).toEqual([open])
  const groups = screen.getAllByRole('region')
  // blocked has no task, so it has no group
  expect(groups.map((g) => g.getAttribute('aria-label'))).toEqual(['running', 'queued', 'in_review', 'draft'])
  expect(groups.map((g) => within(g).getByRole('heading')).map((h) => h.textContent?.replace(/\D/g, ''))).toEqual([
    '2',
    '1',
    '1',
    '1',
  ])
  const running = within(groups[0]).getAllByRole('link')
  expect(running.map((l) => l.getAttribute('href'))).toEqual(['/tasks/2', '/tasks/4'])
  expect(within(running[1]).getByText('Task 4')).toBeInTheDocument()
  expect(within(groups[3]).getByRole('link')).toHaveAttribute('href', '/tasks/1')
})

test('Status asks for finished tasks only when ticked', async () => {
  const calls = serve([row(1, 'running'), row(2, 'draft'), row(3, 'done'), row(4, 'cancelled')])
  await show()
  expect(listReads(calls)).toEqual([open])
  expect(screen.getByRole('checkbox')).not.toBeChecked()
  expect(groupNames()).toEqual(['running', 'draft'])

  fireEvent.click(screen.getByRole('checkbox'))
  await act(() => vi.advanceTimersByTimeAsync(0))
  expect(listReads(calls).slice(1)).toEqual([
    open,
    '/api/tasks?status=done&order=newest&limit=30',
    '/api/tasks?status=cancelled&order=newest&limit=30',
  ])
  expect(groupNames()).toEqual(['running', 'draft', 'done', 'cancelled'])
  expect(within(group('done')).getByRole('link')).toHaveAttribute('href', '/tasks/3')
  expect(within(group('cancelled')).getByRole('link')).toHaveAttribute('href', '/tasks/4')

  fireEvent.click(screen.getByRole('checkbox'))
  await act(() => vi.advanceTimersByTimeAsync(0))
  expect(listReads(calls).slice(4)).toEqual([open])
  expect(groupNames()).toEqual(['running', 'draft'])
})

test('a ticked status address asks for finished tasks from the start', async () => {
  const calls = serve([])
  await show('/status?finished=1')
  expect(listReads(calls)).toHaveLength(3)
  expect(screen.getByRole('checkbox')).toBeChecked()
})

test('a full finished group says it shows the newest', async () => {
  const done = Array.from({ length: 30 }, (_, i) => row(i + 1, 'done'))
  serve([...done, row(31, 'cancelled'), row(32, 'cancelled')])
  await show('/status?finished=1')
  expect(within(group('done')).getByRole('heading')).toHaveTextContent('newest 30')
  expect(within(group('cancelled')).getByRole('heading')).not.toHaveTextContent('newest')
  expect(within(group('cancelled')).getByRole('heading')).toHaveTextContent('2')
})
