import { act, render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import App from '../../App.tsx'
import type { Row, Status } from '../../api/types.ts'
import { fakeServer, json, narrow } from '../../test-server.ts'

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

test('Status groups the same tasks on a phone', async () => {
  const calls = fakeServer((c) => {
    if (c.path === '/api/tasks?needs_you=true') return json({ tasks: [] }) // the frame's count
    if (c.path.startsWith('/api/tasks?status=')) {
      return json({
        tasks: [
          row(1, 'draft'),
          row(2, 'running'),
          row(3, 'in_review'),
          row(4, 'running', { needs_you: true, waiting: 'Waiting for an answer.' }),
          row(5, 'queued'),
        ],
      })
    }
    return undefined
  })
  vi.useFakeTimers()
  narrow(true)
  render(
    <MemoryRouter initialEntries={['/status']}>
      <App />
    </MemoryRouter>,
  )
  await act(() => vi.advanceTimersByTimeAsync(0))

  expect(calls.filter((c) => c.path.startsWith('/api/tasks?status=')).map((c) => c.path)).toEqual([
    '/api/tasks?status=draft,queued,running,blocked,in_review',
  ])
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
