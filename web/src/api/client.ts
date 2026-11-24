export class ApiError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

let unauthorized: () => void = () => {}
let redirected: () => void = () => {}

// One handler, replaced by a later call.
export function onUnauthorized(handler: () => void) {
  unauthorized = handler
}

// One handler, replaced by a later call.
export function onRedirect(handler: () => void) {
  redirected = handler
}

// A file or a blob is sent as it is, under its own type; any other body as JSON.
export async function api<T>(method: string, path: string, body?: unknown, options: { keepalive?: boolean; signal?: AbortSignal } = {}): Promise<T> {
  const headers: Record<string, string> = {}
  const raw = body instanceof Blob
  if (body !== undefined) headers['Content-Type'] = raw ? body.type || 'application/octet-stream' : 'application/json'
  if (method !== 'GET' && method !== 'HEAD') headers['X-CAVU-CSRF'] = '1'
  const res = await fetch('/api' + path, {
    method,
    headers,
    credentials: 'same-origin',
    // With the default follow, the page's policy refuses a redirect to another origin and fetch fails as if offline.
    redirect: 'manual',
    keepalive: options.keepalive,
    signal: options.signal,
    body: raw ? body : body === undefined ? undefined : JSON.stringify(body),
  })
  if (res.type === 'opaqueredirect') {
    redirected()
    throw new ApiError(0, 'Access to this page has ended. Reload to sign in again.')
  }
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
