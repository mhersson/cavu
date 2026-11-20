export class ApiError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

let unauthorized: () => void = () => {}

// One handler, replaced by a later call.
export function onUnauthorized(handler: () => void) {
  unauthorized = handler
}

export async function api<T>(method: string, path: string, body?: unknown, options: { keepalive?: boolean } = {}): Promise<T> {
  const headers: Record<string, string> = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (method !== 'GET' && method !== 'HEAD') headers['X-CAVU-CSRF'] = '1'
  const res = await fetch('/api' + path, {
    method,
    headers,
    credentials: 'same-origin',
    keepalive: options.keepalive,
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (res.status === 204) return undefined as T
  if (res.status >= 400) {
    // A refused sign-in is the sign-in page's to show.
    if (res.status === 401 && !(method === 'POST' && path === '/auth/login')) unauthorized()
    let message = res.statusText || `HTTP ${res.status}`
    try {
      const e = (await res.json()) as { error?: unknown }
      if (typeof e.error === 'string') message = e.error
    } catch {
      // the body is not the server's JSON
    }
    throw new ApiError(res.status, message)
  }
  return (await res.json()) as T
}
