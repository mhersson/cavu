import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { MemoryRouter } from 'react-router'
import type { Change, PullRequest, Question, Stop, Task } from '../api/types.ts'
import { fakeServer, json, type Call } from '../test-server.ts'
import NeedsBox from './NeedsBox.tsx'

afterEach(() => vi.unstubAllGlobals())

function task(more: Partial<Task>): Task {
  return {
    id: 7,
    project: 'web',
    title: 'Add a footer',
    body: '',
    requester: 'api',
    created_at: '2030-01-02T03:04:05Z',
    status: 'blocked',
    needs_you: true,
    needs: 'answer',
    needs_you_since: '2030-01-02T04:00:00Z',
    waiting: '',
    waiting_until: null,
    dependencies: [],
    stop: null,
    questions: [],
    review: null,
    pull_request: null,
    change: null,
    turns: 0,
    duration_ms: 0,
    cost_estimate_usd: 0,
    cost_missing_sessions: 0,
    usage: [],
    budget: { turns: 0, max_turns: 0, minutes: 0, max_minutes: 0 },
    ...more,
  }
}

const stop = (reason: Stop['reason'], action: Stop['action'] = 'retry'): Stop => ({
  reason,
  explanation: 'The verify command failed.',
  action,
  resets_at: null,
})

const asking = (id: number, questions: Question[]) =>
  task({ id, needs: 'answer', stop: stop('question', 'answer'), questions })

const pr: PullRequest = {
  number: 12,
  url: 'https://example.test/pull/12',
  branch: 'cavu/7',
  head: '0123456789abcdef',
  state: 'open',
  checks: 'passed',
  failed_checks: [],
  ready: true,
}

const change: Change = {
  head_commit: 'fedcba9876543210',
  files_changed: 3,
  diff_stat: [' a | 1 +', ' 1 file changed, 1 insertion(+)'],
  flagged: { workflows: ['.github/ci.yaml'], instructions: [], large: [] },
}

const stopped = task({ needs: 'retry', stop: stop('ci_failed') })
const done = task({ status: 'queued', needs_you: false, needs: '', needs_you_since: null })

const isAction = (c: Call) => c.method === 'POST' && /^\/api\/tasks\/\d+\/actions$/.test(c.path)
const actions = (calls: Call[]) => calls.filter(isAction).map((c) => c.body)
const isEvents = (c: Call) => c.path.startsWith('/api/tasks/7/events')

// act answers each action in turn, the last one again once they run out.
function serve(reread: Task, ...act: (() => Response)[]) {
  let n = 0
  return fakeServer((c) => {
    if (isAction(c)) return act[Math.min(n++, act.length - 1)]()
    if (c.method === 'GET' && /^\/api\/tasks\/\d+$/.test(c.path)) return json(reread)
  })
}

function show(t: Task, props: { onPage?: boolean; flat?: boolean } = {}) {
  const onTask = vi.fn()
  const view = render(
    <MemoryRouter>
      <NeedsBox task={t} onTask={onTask} {...props} />
    </MemoryRouter>,
  )
  return { onTask, ...view }
}

const answerBox = () => screen.getByRole('textbox', { name: 'Answer' })
const button = (name: string) => screen.getByRole('button', { name })
const link = (name: string) => screen.getByRole('link', { name })

test('a question shows every question and sends the answer as one text', async () => {
  const qs = [
    { question: 'Which pages should the footer link to?', suggested_answer: 'Privacy and contact.' },
    { question: 'Should it show the year?', suggested_answer: 'Yes.' },
  ]
  const calls = serve(done, () => json(done))
  const { onTask } = show(asking(1, qs))
  const items = screen.getAllByRole('listitem')
  expect(items).toHaveLength(2)
  qs.forEach((q, i) => {
    expect(items[i]).toHaveTextContent(q.question)
    expect(items[i]).toHaveTextContent(q.suggested_answer)
  })
  const user = userEvent.setup()
  await user.type(answerBox(), 'Privacy only.{Enter}No year.')
  await user.click(button('Answer'))
  await waitFor(() => expect(onTask).toHaveBeenCalledWith(done))
  expect(actions(calls)).toEqual([{ action: 'answer', text: 'Privacy only.\nNo year.' }])
})

test('"Use its assumptions" fills the box and sends nothing', async () => {
  const calls = serve(done, () => json(done))
  const user = userEvent.setup()
  const several = show(
    asking(2, [
      { question: 'Which pages?', suggested_answer: 'Privacy and contact.' },
      { question: 'A year?', suggested_answer: '' },
      { question: 'A version?', suggested_answer: 'The build version.' },
    ]),
  )
  await user.click(button('Use its assumptions'))
  expect(answerBox()).toHaveValue('1. Privacy and contact.\n3. The build version.')
  several.unmount()

  const one = show(asking(3, [{ question: 'Which pages?', suggested_answer: 'Privacy and contact.' }]))
  await user.click(button('Use its assumptions'))
  expect(answerBox()).toHaveValue('Privacy and contact.')
  one.unmount()

  show(asking(4, [{ question: 'Which pages?', suggested_answer: '' }]))
  expect(screen.queryByRole('button', { name: 'Use its assumptions' })).toBeNull()
  expect(calls).toHaveLength(0)
})

test('Retry sends retry', async () => {
  const calls = serve(stopped, () => json(done))
  const { onTask } = show(stopped)
  await userEvent.setup().click(button('Retry'))
  await waitFor(() => expect(onTask).toHaveBeenCalledWith(done))
  expect(actions(calls)).toEqual([{ action: 'retry' }])
})

test('a ready pull request links to its address', () => {
  serve(done)
  const { container } = show(task({ status: 'in_review', needs: 'review', pull_request: pr, change }))
  const open = link('Open pull request')
  expect(open).toHaveAttribute('href', pr.url)
  expect(open).toHaveAttribute('target', '_blank')
  expect(open).toHaveAttribute('rel', 'noreferrer')
  expect(container.querySelectorAll('.flag')).toHaveLength(1)
})

test('a change to take offers the download and the recorded merge', async () => {
  const calls = serve(done, () => json(done))
  const { onTask } = show(task({ status: 'in_review', needs: 'take', change }))
  expect(link('Download the change')).toHaveAttribute('href', '/api/tasks/7/change?format=bundle')
  expect(link('Download the change')).toHaveAttribute('download')
  expect(link('Diff')).toHaveAttribute('href', '/api/tasks/7/change?format=diff')
  expect(link('Diff')).toHaveAttribute('download')
  await userEvent.setup().click(button('Record the merge'))
  await waitFor(() => expect(onTask).toHaveBeenCalledWith(done))
  expect(actions(calls)).toEqual([{ action: 'done' }])
})

test("a refused action shows the server's line and reads the task again", async () => {
  const line = 'events kept being recorded on task 7'
  const reread = { ...stopped, waiting: 'read again' }
  const calls = serve(reread, () => json({ error: line }, 409))
  const { onTask } = show(stopped)
  const user = userEvent.setup()
  await user.click(button('Retry'))
  expect(await screen.findByRole('alert')).toHaveTextContent(line)
  await waitFor(() => expect(onTask).toHaveBeenCalledWith(reread, true))
  const post = calls.findIndex(isAction)
  expect(calls.findIndex((c, i) => i > post && c.method === 'GET' && c.path === '/api/tasks/7')).toBeGreaterThan(post)
  await waitFor(() => expect(button('Retry')).toBeEnabled())
  await user.click(button('Retry'))
  await waitFor(() => expect(actions(calls)).toHaveLength(2))
})

test('a refusal stays in view when the task needs nothing more', async () => {
  const line = 'the question was answered a moment ago'
  const answered = { ...done, id: 8 }
  serve(answered, () => json({ error: line }, 400))
  const onTask = vi.fn()
  // The box's parent shows the task it is handed, as a page does.
  function Live() {
    const [t, setT] = useState(asking(8, [{ question: 'Which pages?', suggested_answer: '' }]))
    const hand = (next: Task, refused?: boolean) => {
      onTask(next, refused)
      setT(next)
    }
    return <NeedsBox flat task={t} onTask={hand} />
  }
  render(
    <MemoryRouter>
      <Live />
    </MemoryRouter>,
  )
  await userEvent.setup().click(button('Answer'))
  await waitFor(() => expect(onTask).toHaveBeenCalledWith(answered, true))
  expect(screen.getByRole('alert')).toHaveTextContent(line)
  expect(link(answered.title)).toHaveAttribute('href', '/tasks/8')
  expect(screen.queryByRole('button')).toBeNull()
})

test('a typed answer survives a failed send', async () => {
  const t = asking(5, [{ question: 'Which pages?', suggested_answer: '' }])
  serve(t, () => json({ error: 'the text is blank' }, 400), () => json(done))
  const user = userEvent.setup()
  const first = show(t)
  await user.type(answerBox(), 'Privacy only.')
  await user.click(button('Answer'))
  await screen.findByRole('alert')
  expect(answerBox()).toHaveValue('Privacy only.')
  first.unmount()

  const second = show(t)
  expect(answerBox()).toHaveValue('Privacy only.')
  await user.click(button('Answer'))
  await waitFor(() => expect(second.onTask).toHaveBeenCalledWith(done))
  second.unmount()

  show(t)
  expect(answerBox()).toHaveValue('')
})

test('the box follows needs', () => {
  serve(done)
  show(
    task({
      status: 'in_review',
      needs: 'take',
      stop: stop('ci_failed'),
      pull_request: pr,
      change,
      questions: [{ question: 'Which pages?', suggested_answer: 'All.' }],
    }),
  )
  expect(button('Record the merge')).toBeInTheDocument()
  expect(link('Download the change')).toBeInTheDocument()
  for (const name of ['Retry', 'Answer', 'Use its assumptions']) {
    expect(screen.queryByRole('button', { name })).toBeNull()
  }
  expect(screen.queryByRole('link', { name: 'Open pull request' })).toBeNull()
  expect(screen.queryByRole('textbox')).toBeNull()
})

test("on the task's page the box leaves out the title and Open task", () => {
  serve(done)
  const listed = show(stopped)
  expect(link(stopped.title)).toHaveAttribute('href', '/tasks/7')
  expect(link('Open task')).toHaveAttribute('href', '/tasks/7')
  listed.unmount()

  show(stopped, { onPage: true })
  expect(screen.queryByRole('link', { name: stopped.title })).toBeNull()
  expect(screen.queryByRole('link', { name: 'Open task' })).toBeNull()
  expect(button('Retry')).toBeInTheDocument()
})

test("the pane shows the failed verify's output", async () => {
  const output = ' FAIL  src/a.test.ts > formats a reset\nAssertionError: expected 1 to be 2'
  const verify = (id: number, out: string) => ({
    id,
    task_id: 7,
    run_id: 3,
    at: '2030-01-02T03:59:00Z',
    type: 'verify_ended',
    actor: 'runner',
    payload: { result: 'failed', command: 'npm test', exit_code: 1, duration_ms: 226000, output: out },
    text: '',
  })
  const calls = fakeServer((c) => {
    if (isEvents(c)) {
      return json({ events: [{ ...verify(9, ''), type: 'stopped', payload: {} }, verify(8, output), verify(5, 'older')] })
    }
  })
  const failed = task({ needs: 'retry', stop: stop('verify_failed') })
  const pane = show(failed, { flat: true })
  expect(await screen.findByText((_, e) => e?.tagName === 'PRE' && e.textContent === output)).toBeInTheDocument()
  expect(screen.getByText('npm test')).toBeInTheDocument()
  const asked = calls.filter(isEvents)
  expect(asked).toHaveLength(1)
  const q = new URL(asked[0].path, 'http://cavu.test').searchParams
  expect([q.get('order'), q.get('agent'), q.get('limit')]).toEqual(['newest', 'false', '10'])
  pane.unmount()

  const box = show(failed)
  box.unmount()
  show(stopped, { flat: true })
  expect(calls.filter(isEvents)).toHaveLength(1)
})
