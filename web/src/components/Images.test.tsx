import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import App from '../App.tsx'
import type { Image, Task } from '../api/types.ts'
import { fakeServer, json, narrow, type Call } from '../test-server.ts'

afterEach(() => vi.unstubAllGlobals())

const shot: Image = { id: 3, name: 'save-button.png', type: 'image/png', size: 190464 }
const wide: Image = { id: 4, name: 'settings-clipped.png', type: 'image/png', size: 94208 }
const CAP = 100

function task(more: Partial<Task> = {}): Task {
  return {
    id: 7,
    project: 'web',
    title: 'Fix the Save button',
    body: 'The label hides the button.',
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
    images: [],
    ...more,
  }
}

const isUpload = (c: Call) => c.method === 'POST' && c.path.startsWith('/api/tasks/7/images?')
const uploads = (calls: Call[]) => calls.filter(isUpload)
const reads = (calls: Call[]) => calls.filter((c) => c.method === 'GET' && c.path === '/api/tasks/7').length

// The server's answer to an upload: the image, now the task's last.
function attach(t: Task, c: Call) {
  const f = c.body as File
  const image = { id: 100 + t.images.length, name: new URL(c.path, 'http://here').searchParams.get('name')!, type: f.type, size: f.size }
  t.images = [...t.images, image]
  return json(image, 201)
}

// The server's answer to a removal: the task without the images it names.
function detach(t: Task, c: Call) {
  const gone = (c.body as { remove_images: number[] }).remove_images
  t.images = t.images.filter((i) => !gone.includes(i.id))
  return json(t)
}

// A server that keeps the task, attaches and removes its images as the server does, and answers with answer first when it gives one.
function serve(t: Task, answer: (c: Call) => Response | Promise<Response> | undefined = () => undefined) {
  return fakeServer((c) => {
    const a = answer(c)
    if (a) return a
    if (c.method === 'GET' && c.path === '/api/settings') return json({ image_max_bytes: CAP })
    if (c.method === 'GET' && c.path === '/api/tasks/7') return json(t)
    if (isUpload(c)) return attach(t, c)
    if (c.method === 'PATCH' && c.path === '/api/tasks/7') return detach(t, c)
    if (c.path === '/api/tasks?needs_you=true') return json({ tasks: [] })
  })
}

function open() {
  render(
    <MemoryRouter initialEntries={['/tasks/7']}>
      <App />
    </MemoryRouter>,
  )
}

const section = () => document.querySelector<HTMLElement>('.task-page .sect')
const tiles = () => [...section()!.querySelectorAll<HTMLElement>('.tile')]
const busy = () => section()!.querySelectorAll('[aria-busy="true"]')
const sources = () => [...section()!.querySelectorAll('img')].map((i) => i.getAttribute('src'))
const picker = () => section()!.querySelector<HTMLInputElement>('input[type=file]:not([capture])')!
const alert = () => within(section()!).queryByRole('alert')
const page = () => document.querySelector<HTMLElement>('.task-page')!
const png = (name: string, bytes: number) => new File([new Uint8Array(bytes)], name, { type: 'image/png' })
const start = () => screen.getByRole('button', { name: 'Start' })
const starts = (calls: Call[]) => calls.filter((c) => c.method === 'POST' && c.path === '/api/tasks/7/actions')
const patches = (calls: Call[]) => calls.filter((c) => c.method === 'PATCH')
const removeButton = (name: string) => within(section()!).queryByRole('button', { name: `Remove ${name}` })
const askToRemove = async (name: string) => {
  await userEvent.click(removeButton(name)!)
  await userEvent.click(within(screen.getByRole('dialog')).getAllByRole('button')[1])
}
const files = (list: File[]) => ({ dataTransfer: { types: ['Files'], files: list } })
const dropOn = (el: HTMLElement, list: File[]) => {
  fireEvent.dragOver(el, files(list))
  fireEvent.drop(el, files(list))
}

test("a draft's images show from their addresses, and each opens the viewer, which downloads and closes", async () => {
  narrow(false)
  serve(task({ images: [shot, wide] }))
  open()
  await waitFor(() => expect(tiles()).toHaveLength(2))
  expect(sources()).toEqual(['/api/tasks/7/images/3', '/api/tasks/7/images/4'])

  await userEvent.click(within(tiles()[1]).getByRole('button', { name: wide.name }))
  const viewer = screen.getByRole('dialog', { name: wide.name })
  expect(viewer.querySelector('img')).toHaveAttribute('src', '/api/tasks/7/images/4')
  expect(within(viewer).getByRole('link')).toHaveAttribute('href', '/api/tasks/7/images/4?download=1')
  await userEvent.click(within(viewer).getByRole('button', { name: 'Close' }))
  expect(screen.queryByRole('dialog')).toBeNull()
})

test('a task past its draft shows its images with nothing to attach or remove, takes no paste, and shows no section without images', async () => {
  narrow(false)
  const calls = serve(task({ status: 'running', images: [shot] }))
  open()
  await waitFor(() => expect(tiles()).toHaveLength(1))
  expect(within(section()!).getAllByRole('button')).toHaveLength(1)
  expect(section()!.querySelector('input')).toBeNull()
  fireEvent.paste(document, { clipboardData: { files: [png('image.png', 4)], types: ['Files'] } })
  dropOn(page(), [png('dropped.png', 4)])
  expect(uploads(calls)).toEqual([])

  cleanup()
  serve(task({ status: 'running' }))
  open()
  await waitFor(() => expect(page().querySelector('.task-meta')).not.toBeNull())
  expect(section()).toBeNull()
})

test('a chosen image is sent as the body under its name, shows a moving bar and holds Start until the server answers, then shows as a tile', async () => {
  narrow(false)
  const t = task()
  let answer = () => {}
  const calls = serve(t, (c) => (isUpload(c) ? new Promise<Response>((r) => (answer = () => r(attach(t, c)))) : undefined))
  open()
  await waitFor(() => expect(picker()).not.toBeNull())
  const chosen = png('shot one.png', 10)
  await userEvent.upload(picker(), chosen)
  const [sent] = uploads(calls)
  expect(sent.path).toBe('/api/tasks/7/images?name=shot+one.png')
  expect(sent.body).toBe(chosen)
  expect(sent.headers.get('Content-Type')).toBe('image/png')
  expect(picker().value).toBe('')
  expect(busy()).toHaveLength(1)
  expect(sources()).toEqual([])
  expect(screen.getByRole('button', { name: 'Start' })).toBeDisabled()

  const before = reads(calls)
  await act(async () => answer())
  await waitFor(() => expect(sources()).toEqual(['/api/tasks/7/images/100']))
  expect(busy()).toHaveLength(0)
  expect(screen.getByRole('button', { name: 'Start' })).toBeEnabled()
  expect(reads(calls)).toBe(before + 1)
})

test('an image over the cap, or of a type other than the four, is refused before it is sent; one the browser gives no type goes', async () => {
  narrow(false)
  const calls = serve(task())
  open()
  await waitFor(() => expect(picker()).not.toBeNull())
  await userEvent.upload(picker(), png('big.png', CAP + 1))
  expect(alert()).not.toBeNull()
  expect(uploads(calls)).toEqual([])

  await userEvent.upload(picker(), png('fits.png', CAP))
  await waitFor(() => expect(tiles()).toHaveLength(1))
  expect(alert()).toBeNull()

  dropOn(page(), [new File([new Uint8Array(4)], 'shot.heic', { type: 'image/heic' })])
  expect(alert()).not.toBeNull()
  expect(uploads(calls)).toHaveLength(1)

  dropOn(page(), [new File([new Uint8Array(4)], 'shot', { type: '' })])
  expect(uploads(calls)).toHaveLength(2)
})

test("the server's refusal of an upload or a removal shows as its line, and the images stay as the server has them", async () => {
  narrow(false)
  serve(task({ images: [shot] }), (c) => (isUpload(c) ? json({ error: 'the task has started' }, 409) : undefined))
  open()
  await waitFor(() => expect(tiles()).toHaveLength(1))
  await userEvent.upload(picker(), png('late.png', 4))
  await waitFor(() => expect(alert()).not.toBeNull())
  expect(tiles()).toHaveLength(1)

  cleanup()
  const removals = serve(task({ images: [shot] }), (c) => (c.method === 'PATCH' ? json({ error: 'the task has started' }, 409) : undefined))
  open()
  await waitFor(() => expect(tiles()).toHaveLength(1))
  expect(alert()).toBeNull()
  await userEvent.click(within(tiles()[0]).getAllByRole('button').at(-1)!)
  await userEvent.click(within(screen.getByRole('dialog')).getAllByRole('button')[1])
  expect(removals.filter((c) => c.method === 'PATCH')).toHaveLength(1)
  await waitFor(() => expect(alert()).not.toBeNull())
  expect(tiles()).toHaveLength(1)
})

test("the × asks first: keeping it sends nothing, and Remove sends the edit with the image's id", async () => {
  narrow(false)
  const calls = serve(task({ images: [shot, wide] }))
  open()
  const remove = async (i: number) => userEvent.click(within(await waitFor(() => tiles()[i])).getAllByRole('button').at(-1)!)
  await remove(1)
  await userEvent.click(within(screen.getByRole('dialog')).getAllByRole('button')[0])
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(calls.filter((c) => c.method === 'PATCH')).toEqual([])

  await remove(1)
  await userEvent.click(within(screen.getByRole('dialog')).getAllByRole('button')[1])
  expect(calls.filter((c) => c.method === 'PATCH').map((c) => [c.path, c.body])).toEqual([['/api/tasks/7', { remove_images: [4] }]])
  await waitFor(() => expect(sources()).toEqual(['/api/tasks/7/images/3']))
})

test('a removal holds Start on a desktop and on a phone, and its image stays without an × until the server removes it', async () => {
  for (const phone of [false, true]) {
    narrow(phone)
    const t = task({ images: [shot, wide] })
    let answer = () => {}
    const calls = serve(t, (c) => (c.method === 'PATCH' ? new Promise<Response>((r) => (answer = () => r(detach(t, c)))) : undefined))
    open()
    await waitFor(() => expect(tiles()).toHaveLength(2))
    expect(start()).toBeEnabled()

    await askToRemove(shot.name)
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(patches(calls)).toHaveLength(1)
    expect(sources()).toEqual(['/api/tasks/7/images/3', '/api/tasks/7/images/4'])
    expect(busy()).toHaveLength(1)
    expect(removeButton(shot.name)).toBeNull()
    expect(start()).toBeDisabled()
    await userEvent.click(start())
    expect(starts(calls)).toEqual([])

    await act(async () => answer())
    await waitFor(() => expect(sources()).toEqual(['/api/tasks/7/images/4']))
    expect(busy()).toHaveLength(0)
    expect(start()).toBeEnabled()
    cleanup()
  }
})

test("a refused removal keeps its image and shows the server's line, and Start comes back once no upload or removal is under way", async () => {
  for (const phone of [false, true]) {
    narrow(phone)
    const t = task({ images: [shot] })
    let refuse = () => {}
    let attached = () => {}
    serve(t, (c) => {
      if (c.method === 'PATCH') return new Promise<Response>((r) => (refuse = () => r(json({ error: 'the task has started' }, 409))))
      if (isUpload(c)) return new Promise<Response>((r) => (attached = () => r(attach(t, c))))
    })
    open()
    await waitFor(() => expect(tiles()).toHaveLength(1))
    await askToRemove(shot.name)
    expect(start()).toBeDisabled()
    await userEvent.upload(picker(), png('more.png', 4))
    expect(busy()).toHaveLength(2)

    await act(async () => refuse())
    await waitFor(() => expect(alert()).not.toBeNull())
    expect(sources()).toEqual(['/api/tasks/7/images/3'])
    expect(removeButton(shot.name)).not.toBeNull()
    expect(busy()).toHaveLength(1)
    expect(start()).toBeDisabled()

    await act(async () => attached())
    await waitFor(() => expect(sources()).toEqual(['/api/tasks/7/images/3', '/api/tasks/7/images/101']))
    expect(busy()).toHaveLength(0)
    expect(start()).toBeEnabled()
    cleanup()
  }
})

test('on a desktop a file dropped on the page and an image pasted are attached, and a paste in a field that holds text is the field’s', async () => {
  narrow(false)
  const calls = serve(task())
  open()
  await waitFor(() => expect(section()).not.toBeNull())
  const dropped = png('dropped.png', 4)
  dropOn(page(), [dropped])
  const pasted = png('image.png', 4)
  fireEvent.paste(document, { clipboardData: { files: [pasted], types: ['Files'] } })
  const text = fireEvent.paste(document, { clipboardData: { files: [], types: ['text/plain'] } })

  await userEvent.click(screen.getByRole('button', { name: 'Edit' }))
  const cells = fireEvent.paste(screen.getByRole('textbox', { name: 'Details' }), {
    clipboardData: { files: [png('cells.png', 4)], types: ['text/plain', 'Files'] },
  })
  expect(uploads(calls).map((c) => c.body)).toEqual([dropped, pasted])
  expect([text, cells]).toEqual([true, true])
})

test('on a desktop Attach an image opens the picker of the four types', async () => {
  narrow(false)
  serve(task())
  open()
  const clicked = vi.spyOn(HTMLInputElement.prototype, 'click').mockImplementation(() => {})
  try {
    await userEvent.click(await waitFor(() => within(section()!).getByRole('button', { name: 'Attach an image' })))
    expect(clicked.mock.contexts.map((i) => [(i as HTMLInputElement).accept, (i as HTMLInputElement).getAttribute('capture')])).toEqual([
      ['image/png,image/jpeg,image/gif,image/webp', null],
    ])
  } finally {
    clicked.mockRestore()
  }
})

test('on a phone Take a photo opens the camera and Choose an image the picker of the four types, and a paste attaches nothing', async () => {
  narrow(true)
  const calls = serve(task())
  open()
  const clicked = vi.spyOn(HTMLInputElement.prototype, 'click').mockImplementation(() => {})
  try {
    await waitFor(() => expect(section()).not.toBeNull())
    await userEvent.click(within(section()!).getByRole('button', { name: 'Take a photo' }))
    await userEvent.click(within(section()!).getByRole('button', { name: 'Choose an image' }))
    expect(clicked.mock.contexts.map((i) => [(i as HTMLInputElement).accept, (i as HTMLInputElement).getAttribute('capture')])).toEqual([
      ['image/*', 'environment'],
      ['image/png,image/jpeg,image/gif,image/webp', null],
    ])
  } finally {
    clicked.mockRestore()
  }
  fireEvent.paste(document, { clipboardData: { files: [png('image.png', 4)], types: ['Files'] } })
  expect(uploads(calls)).toEqual([])
})

test('leaving the task ends an upload under way', async () => {
  narrow(false)
  const calls = serve(task(), (c) => (isUpload(c) ? new Promise<Response>(() => {}) : undefined))
  open()
  await waitFor(() => expect(picker()).not.toBeNull())
  await userEvent.upload(picker(), png('slow.png', 4))
  const [sent] = uploads(calls)
  expect(sent.signal!.aborted).toBe(false)
  await userEvent.click(within(page().querySelector<HTMLElement>('.crumb')!).getByRole('link'))
  await waitFor(() => expect(page()).toBeNull())
  expect(sent.signal!.aborted).toBe(true)
})
