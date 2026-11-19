import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import { fakeServer, json } from '../../test-server.ts'
import Login from './Login.tsx'

afterEach(() => vi.unstubAllGlobals())

test('sign-in sends name and password and shows a refusal', async () => {
  const calls = fakeServer((c) => {
    if (c.path !== '/api/auth/login') return
    return (c.body as { name: string }).name === 'slow' ? json({ error: 'wait a while' }, 429) : json({ error: 'refused here' }, 401)
  })
  render(
    <MemoryRouter>
      <Login />
    </MemoryRouter>,
  )
  const user = userEvent.setup()
  const name = screen.getByRole('textbox')
  const password = document.querySelector<HTMLInputElement>('input[type=password]')!
  await user.type(name, 'ada')
  await user.type(password, 'secret')
  await user.click(screen.getByRole('button'))

  const login = calls.find((c) => c.path === '/api/auth/login')!
  expect(login.method).toBe('POST')
  expect(login.body).toEqual({ name: 'ada', password: 'secret' })
  expect(login.headers.get('X-CAVU-CSRF')).toBe('1')
  expect(await screen.findByRole('alert')).toHaveTextContent('refused here')
  expect(name).toHaveValue('ada')

  await user.clear(name)
  await user.type(name, 'slow')
  await user.click(screen.getByRole('button'))
  await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('wait a while'))
  expect(calls.filter((c) => c.path === '/api/auth/login')).toHaveLength(2)
})
