import { ApiError, api, onUnauthorized } from './client.ts'

function fakeServer(status: number, body?: unknown) {
  const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
    status === 204
      ? new Response(null, { status })
      : new Response(JSON.stringify(body), { status, statusText: 'Status text' }),
  )
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function headersOf(fetchMock: ReturnType<typeof fakeServer>) {
  return new Headers(fetchMock.mock.calls[0][1]?.headers)
}

afterEach(() => {
  vi.unstubAllGlobals()
  onUnauthorized(() => {})
})

test('client sends the CSRF header on a write and not on a read', async () => {
  const read = fakeServer(200, {})
  await api('GET', '/tasks')
  expect(read.mock.calls[0][0]).toBe('/api/tasks')
  expect(headersOf(read).has('X-CAVU-CSRF')).toBe(false)

  const write = fakeServer(200, {})
  await api('POST', '/tasks', { title: 'x' })
  expect(headersOf(write).get('X-CAVU-CSRF')).toBeTruthy()
  expect(headersOf(write).get('Content-Type')).toBe('application/json')
  const init = write.mock.calls[0][1]
  expect(init?.body).toBe('{"title":"x"}')
  expect(init?.credentials).toBe('same-origin')

  const del = fakeServer(204)
  await expect(api('DELETE', '/projects/1')).resolves.toBeUndefined()
  expect(headersOf(del).get('X-CAVU-CSRF')).toBeTruthy()
})

test("client throws the server's line", async () => {
  fakeServer(400, { error: 'title is required' })
  const err = await api('POST', '/tasks', {}).catch((e: unknown) => e)
  expect(err).toBeInstanceOf(ApiError)
  expect(err).toMatchObject({ status: 400, message: 'title is required' })

  fakeServer(502, undefined)
  const plain = await api('GET', '/tasks').catch((e: unknown) => e)
  expect(plain).toMatchObject({ status: 502, message: 'Status text' })
})

test('client names the status when a non-JSON body comes with no status text', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response('<html>Bad Gateway</html>', { status: 502 })))
  await expect(api('GET', '/tasks')).rejects.toMatchObject({ status: 502, message: 'HTTP 502' })
})

test('a 401 calls the registered handler', async () => {
  const handler = vi.fn()
  onUnauthorized(handler)
  fakeServer(401, { error: 'sign in' })
  await expect(api('GET', '/tasks')).rejects.toMatchObject({ status: 401 })
  expect(handler).toHaveBeenCalledTimes(1)

  fakeServer(401, { error: 'wrong name or password' })
  await expect(api('POST', '/auth/login', {})).rejects.toMatchObject({ status: 401 })
  expect(handler).toHaveBeenCalledTimes(1)
})
