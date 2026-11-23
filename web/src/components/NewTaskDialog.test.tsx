import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router'
import type { Task } from '../api/types.ts'
import { fakeServer, json, narrow, type Call } from '../test-server.ts'
import NewTask from '../pages/NewTask/NewTask.tsx'
import TaskPage from '../pages/Task/Task.tsx'
import { NewTaskDialog } from './NewTaskDialog.tsx'

afterEach(() => vi.unstubAllGlobals())

// The page makes an address for each image it holds and releases it; jsdom has neither.
const made: [Blob, string][] = []
const released: string[] = []
beforeEach(() => {
  made.length = 0
  released.length = 0
  URL.createObjectURL = (b: Blob) => {
    made.push([b, `blob:${made.length + 1}`])
    return `blob:${made.length}`
  }
  URL.revokeObjectURL = (u: string) => void released.push(u)
})

const CAP = 100

const isCreate = (c: { method: string; path: string }) => c.method === 'POST' && c.path === '/api/tasks'
const isUpload = (c: Call) => c.method === 'POST' && c.path.startsWith('/api/tasks/7/images?')
const isStart = (c: Call) => c.method === 'POST' && c.path === '/api/tasks/7/actions'
const sent = (calls: Call[]) => calls.filter((c) => c.method !== 'GET').map((c) => [c.method, c.path])

// answer comes first when it gives one.
function serve(create: () => Response = () => json({ id: 7 }, 201), answer: (c: Call) => Response | Promise<Response> | undefined = () => undefined) {
  return fakeServer((c) => {
    const a = answer(c)
    if (a) return a
    if (c.path === '/api/projects') return json({ projects: [{ name: 'web' }, { name: 'api' }] })
    if (c.path === '/api/settings') return json({ image_max_bytes: CAP })
    if (isCreate(c)) return create()
    if (isUpload(c)) return json({ id: 1, name: 'x', type: 'image/png', size: 4 }, 201)
    if (isStart(c)) return json({ id: 7 })
  })
}

function open(onClose = vi.fn(), page = <p data-testid="page">task page</p>) {
  const { unmount } = render(
    <MemoryRouter initialEntries={['/']}>
      <Routes>
        <Route path="/" element={<NewTaskDialog onClose={onClose} />} />
        <Route path="/tasks/:id" element={page} />
      </Routes>
    </MemoryRouter>,
  )
  return { onClose, unmount }
}

const form = () => document.querySelector<HTMLElement>('form.new-task')!
const section = () => form().querySelector<HTMLElement>('.sect')!
const names = () => [...section().querySelectorAll('.tile .nm')].map((n) => n.textContent)
const sources = () => [...section().querySelectorAll('img')].map((i) => i.getAttribute('src'))
const busy = () => section().querySelectorAll('[aria-busy="true"]')
const picker = () => section().querySelector<HTMLInputElement>('input[type=file]:not([capture])')!
const camera = () => section().querySelector<HTMLInputElement>('input[type=file][capture]')!
const png = (name: string, bytes = 4) => new File([new Uint8Array(bytes)], name, { type: 'image/png' })
const heic = () => new File([new Uint8Array(4)], 'shot.heic', { type: 'image/heic' })
const files = (list: File[]) => ({ dataTransfer: { types: ['Files'], files: list } })
const dropOn = (el: HTMLElement, list: File[]) => {
  fireEvent.dragOver(el, files(list))
  fireEvent.drop(el, files(list))
}

// The select is there at once; its options come with the projects.
const field = async (label: string) => {
  await screen.findByRole('option', { name: 'web' })
  return screen.getByLabelText(label)
}

test('the dialog creates a draft unless the tick is set', async () => {
  const calls = serve()
  open()
  const user = userEvent.setup()
  await user.type(await field('What should be done?'), 'Add a footer')
  await user.type(await field('Details'), 'Below the fold.')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await screen.findByTestId('page')
  expect(calls.filter(isCreate)[0].body).toEqual({
    project: 'web',
    title: 'Add a footer',
    body: 'Below the fold.',
    depends_on: [],
    start: false,
  })
})

test('a ticked start without images is sent with the create', async () => {
  const calls = serve()
  open()
  const user = userEvent.setup()
  await user.selectOptions(await field('Project'), 'api')
  await user.type(await field('What should be done?'), 'Fix it')
  await user.click(screen.getByRole('checkbox'))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await screen.findByTestId('page')
  expect(calls.filter(isCreate)[0].body).toMatchObject({ project: 'api', start: true })
  expect(sent(calls)).toEqual([['POST', '/api/tasks']])
})

test('the dialog sends depends_on as numbers', async () => {
  const calls = serve()
  open()
  const user = userEvent.setup()
  await user.type(await field('What should be done?'), 'Later')
  await user.type(await field('Depends on'), '42, 43')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await screen.findByTestId('page')
  expect(calls.filter(isCreate)[0].body).toMatchObject({ depends_on: [42, 43] })
})

test("a refused create shows the server's line and keeps what was typed", async () => {
  serve(() => json({ error: 'the project is not runnable' }, 400))
  open()
  const user = userEvent.setup()
  const title = await field('What should be done?')
  await user.type(title, 'Add a footer')
  await user.click(screen.getByRole('checkbox'))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('the project is not runnable')
  expect(title).toHaveValue('Add a footer')
  expect(screen.getByRole('checkbox')).toBeChecked()
  expect(screen.queryByTestId('page')).toBeNull()
})

test("Create opens the new task's page", async () => {
  serve()
  open()
  const user = userEvent.setup()
  await user.type(await field('What should be done?'), 'x')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByTestId('page')).toBeInTheDocument()
})

test('Cancel closes the dialog without a request', async () => {
  const calls = serve()
  const { onClose } = open()
  const user = userEvent.setup()
  await field('Project')
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(onClose).toHaveBeenCalled()
  expect(calls.filter(isCreate)).toHaveLength(0)
})

test('the phone page shows the form with Create alone', async () => {
  serve()
  render(
    <MemoryRouter>
      <NewTask />
    </MemoryRouter>,
  )
  await field('Project')
  expect(screen.getByRole('button', { name: 'Create' })).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull()
})

test('a second click while a create is pending sends no second request', async () => {
  const calls = serve()
  const answer = globalThis.fetch
  let release = () => {}
  const gate = new Promise<void>((r) => (release = r))
  vi.stubGlobal('fetch', (path: string, init?: RequestInit) =>
    init?.method === 'POST' ? gate.then(() => answer(path, init)) : answer(path, init),
  )
  open()
  const user = userEvent.setup()
  await user.type(await field('What should be done?'), 'x')
  const create = screen.getByRole('button', { name: 'Create' })
  await user.click(create)
  await user.click(create)
  release()
  await screen.findByTestId('page')
  expect(calls.filter(isCreate)).toHaveLength(1)
})

// What the history entry holds, which Back, Forward and a reload hand to the page again.
function Entry() {
  return <output data-testid="entry">{JSON.stringify(useLocation().state)}</output>
}
const opened = (
  <>
    <TaskPage />
    <Entry />
  </>
)

// What the task's page reads once the form has opened it.
const draft: Task = {
  id: 7,
  project: 'web',
  title: 'Fix the Save button',
  body: '',
  requester: 'api',
  created_at: '2030-01-02T03:04:05Z',
  status: 'draft',
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
  turns: 0,
  duration_ms: 0,
  cost_estimate_usd: 0,
  cost_missing_sessions: 0,
  usage: [],
  budget: { turns: 0, max_turns: 100, minutes: 0, max_minutes: 60 },
  images: [{ id: 1, name: 'b.png', type: 'image/png', size: 4 }],
}

test('on a desktop the form holds chosen, dropped and pasted images as thumbnails of the files, and an × removes one at once', async () => {
  narrow(false)
  const calls = serve()
  const { unmount } = open()
  const user = userEvent.setup()
  await user.type(await field('What should be done?'), 'Fix it')
  const clicked = vi.spyOn(HTMLInputElement.prototype, 'click').mockImplementation(() => {})
  await user.click(within(section()).getByRole('button', { name: 'Attach an image' }))
  expect(clicked.mock.contexts).toEqual([picker()])
  clicked.mockRestore()

  const chosen = png('chosen.png')
  const dropped = png('dropped.png')
  const pasted = png('image.png')
  await user.upload(picker(), chosen)
  expect(picker().value).toBe('')
  dropOn(form(), [dropped])
  fireEvent.paste(screen.getByLabelText('Details'), { clipboardData: { files: [pasted], types: ['Files'] } })
  expect(names()).toEqual(['chosen.png', 'dropped.png', 'image.png'])
  expect(made).toEqual([
    [chosen, 'blob:1'],
    [dropped, 'blob:2'],
    [pasted, 'blob:3'],
  ])
  expect(sources()).toEqual(['blob:1', 'blob:2', 'blob:3'])

  await user.click(screen.getByRole('button', { name: 'Remove dropped.png' }))
  expect(names()).toEqual(['chosen.png', 'image.png'])
  expect(released).toEqual(['blob:2'])
  expect(sent(calls)).toEqual([])

  unmount()
  expect(released.toSorted()).toEqual(['blob:1', 'blob:2', 'blob:3'])
})

test('the form starts the task only after every upload', async () => {
  narrow(false)
  const answers: (() => void)[] = []
  const calls = serve(undefined, (c) =>
    isUpload(c) ? new Promise<Response>((r) => answers.push(() => r(json({ id: answers.length, name: 'x', type: 'image/png', size: 4 }, 201)))) : undefined,
  )
  open()
  const user = userEvent.setup()
  await user.type(await field('What should be done?'), 'Fix it')
  const a = png('a.png')
  const b = png('b.png')
  await user.upload(picker(), [a, b])
  await user.click(screen.getByRole('checkbox'))
  await user.click(screen.getByRole('button', { name: 'Create' }))

  await waitFor(() => expect(calls.filter(isUpload)).toHaveLength(1))
  expect(calls.filter(isCreate)[0].body).toMatchObject({ start: false })
  expect(calls.filter(isUpload).map((c) => [c.path, c.body])).toEqual([['/api/tasks/7/images?name=a.png', a]])
  expect(busy()).toHaveLength(2)
  fireEvent.paste(screen.getByLabelText('Details'), { clipboardData: { files: [png('late.png')], types: ['Files'] } })
  dropOn(form(), [png('dropped.png')])
  expect(busy()).toHaveLength(2)
  await act(async () => answers[0]())
  await waitFor(() => expect(calls.filter(isUpload)).toHaveLength(2))
  expect(calls.filter(isUpload)[1].body).toBe(b)
  expect(calls.filter(isStart)).toEqual([])

  await act(async () => answers[1]())
  await screen.findByTestId('page')
  expect(sent(calls)).toEqual([
    ['POST', '/api/tasks'],
    ['POST', '/api/tasks/7/images?name=a.png'],
    ['POST', '/api/tasks/7/images?name=b.png'],
    ['POST', '/api/tasks/7/actions'],
  ])
  expect(calls.filter(isStart)[0].body).toEqual({ action: 'start' })
})

test('without the tick the images are uploaded and the task is left a draft', async () => {
  narrow(false)
  const calls = serve()
  open()
  const user = userEvent.setup()
  await user.type(await field('What should be done?'), 'Fix it')
  await user.upload(picker(), png('a.png'))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await screen.findByTestId('page')
  expect(calls.filter(isCreate)[0].body).toMatchObject({ start: false })
  expect(sent(calls)).toEqual([
    ['POST', '/api/tasks'],
    ['POST', '/api/tasks/7/images?name=a.png'],
  ])
})

test("when an upload fails, no start is sent and the task's page opens naming the image that failed", async () => {
  narrow(false)
  const calls = serve(undefined, (c) => {
    if (c.path === '/api/tasks/7/images?name=a.png') return json({ error: 'the image is not one CAVU keeps' }, 415)
    if (c.method === 'GET' && c.path === '/api/tasks/7') return json(draft)
  })
  open(vi.fn(), opened)
  const user = userEvent.setup()
  await user.type(await field('What should be done?'), 'Fix it')
  await user.upload(picker(), [png('a.png'), png('b.png')])
  await user.click(screen.getByRole('checkbox'))
  await user.click(screen.getByRole('button', { name: 'Create' }))

  const line = await waitFor(() => within(document.querySelector<HTMLElement>('.task-page .sect')!).getByRole('alert'))
  expect(line).toHaveTextContent('a.png')
  expect(line).not.toHaveTextContent('b.png')
  await waitFor(() => expect(screen.getByTestId('entry')).toHaveTextContent('null'))
  expect(line).toBeInTheDocument()
  expect(sent(calls)).toEqual([
    ['POST', '/api/tasks'],
    ['POST', '/api/tasks/7/images?name=a.png'],
    ['POST', '/api/tasks/7/images?name=b.png'],
  ])
})

test("a start the server refuses after the uploads opens the task's page with its line", async () => {
  narrow(false)
  serve(undefined, (c) => {
    if (isStart(c)) return json({ error: 'the project is not runnable' }, 409)
    if (c.method === 'GET' && c.path === '/api/tasks/7') return json(draft)
  })
  open(vi.fn(), opened)
  const user = userEvent.setup()
  await user.type(await field('What should be done?'), 'Fix it')
  await user.upload(picker(), png('b.png'))
  await user.click(screen.getByRole('checkbox'))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  const line = await screen.findByRole('alert')
  expect(line).toHaveTextContent('the project is not runnable')
  expect(document.querySelector('.task-page')).not.toBeNull()
  await waitFor(() => expect(screen.getByTestId('entry')).toHaveTextContent('null'))
  expect(line).toBeInTheDocument()
})

test('a HEIC photo is refused before it is sent', async () => {
  narrow(true)
  const calls = serve()
  render(
    <MemoryRouter>
      <NewTask />
    </MemoryRouter>,
  )
  const user = userEvent.setup()
  await user.type(await field('What should be done?'), 'Fix it')
  const clicked = vi.spyOn(HTMLInputElement.prototype, 'click').mockImplementation(() => {})
  await user.click(within(section()).getByRole('button', { name: 'Take a photo' }))
  await user.click(within(section()).getByRole('button', { name: 'Choose an image' }))
  expect(clicked.mock.contexts).toEqual([camera(), picker()])
  clicked.mockRestore()
  expect(camera().accept).toBe('image/*')
  expect(within(section()).queryByRole('button', { name: 'Attach an image' })).toBeNull()

  await user.upload(camera(), heic())
  expect(within(section()).getByRole('alert')).toBeInTheDocument()
  expect(names()).toEqual([])
  await user.upload(picker(), png('big.png', CAP + 1))
  expect(names()).toEqual([])
  await user.upload(picker(), png('fits.png', CAP))
  expect(names()).toEqual(['fits.png'])
  expect(within(section()).queryByRole('alert')).toBeNull()

  fireEvent.paste(screen.getByLabelText('Details'), { clipboardData: { files: [png('image.png')], types: ['Files'] } })
  expect(names()).toEqual(['fits.png'])
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(calls.filter(isUpload)).toHaveLength(1))
  expect(calls.filter(isUpload)[0].path).toBe('/api/tasks/7/images?name=fits.png')
})

test('closing the form while an image uploads stops it and sends nothing more', async () => {
  narrow(false)
  // As fetch does, an aborted upload fails.
  const calls = serve(undefined, (c) =>
    isUpload(c) ? new Promise<Response>((_, fail) => c.signal!.addEventListener('abort', () => fail(new DOMException('aborted', 'AbortError')))) : undefined,
  )
  const { unmount } = open()
  const user = userEvent.setup()
  await user.type(await field('What should be done?'), 'Fix it')
  await user.upload(picker(), [png('a.png'), png('b.png')])
  await user.click(screen.getByRole('checkbox'))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(calls.filter(isUpload)).toHaveLength(1))
  unmount()
  await act(async () => {})
  expect(calls.filter(isUpload)[0].signal!.aborted).toBe(true)
  expect(sent(calls)).toEqual([
    ['POST', '/api/tasks'],
    ['POST', '/api/tasks/7/images?name=a.png'],
  ])
})
