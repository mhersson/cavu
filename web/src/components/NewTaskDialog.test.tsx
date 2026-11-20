import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router'
import { fakeServer, json } from '../test-server.ts'
import NewTask from '../pages/NewTask/NewTask.tsx'
import { NewTaskDialog } from './NewTaskDialog.tsx'

afterEach(() => vi.unstubAllGlobals())

const isCreate = (c: { method: string; path: string }) => c.method === 'POST' && c.path === '/api/tasks'

function serve(create: () => Response = () => json({ id: 7 }, 201)) {
  return fakeServer((c) => {
    if (c.path === '/api/projects') return json({ projects: [{ name: 'web' }, { name: 'api' }] })
    if (isCreate(c)) return create()
  })
}

function open(onClose = vi.fn()) {
  render(
    <MemoryRouter initialEntries={['/']}>
      <Routes>
        <Route path="/" element={<NewTaskDialog onClose={onClose} />} />
        <Route path="/tasks/:id" element={<p data-testid="page">task page</p>} />
      </Routes>
    </MemoryRouter>,
  )
  return onClose
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

test('a ticked start is sent', async () => {
  const calls = serve()
  open()
  const user = userEvent.setup()
  await user.selectOptions(await field('Project'), 'api')
  await user.type(await field('What should be done?'), 'Fix it')
  await user.click(screen.getByRole('checkbox'))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await screen.findByTestId('page')
  expect(calls.filter(isCreate)[0].body).toMatchObject({ project: 'api', start: true })
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
  const onClose = open()
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
