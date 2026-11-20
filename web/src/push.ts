import { useEffect } from 'react'
import { api, ApiError } from './api/client.ts'
import { device, keepDevice, onReopen } from './api/stream.ts'

export type PushState = 'unsupported' | 'install-first' | 'denied' | 'off' | 'on'

const available = () => 'serviceWorker' in navigator && 'PushManager' in window

export function register() {
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {}) // push is then unavailable
}

// "on" means an id is kept; the page brings it in line with the browser's subscription on load.
export function pushState(): PushState {
  if (!available()) {
    // On iOS, navigator.standalone tells a Home Screen app from a Safari tab, and push is the Home Screen app's.
    return window.isSecureContext && (navigator as { standalone?: boolean }).standalone === false ? 'install-first' : 'unsupported'
  }
  if (Notification.permission === 'denied') return 'denied'
  return device() === null ? 'off' : 'on'
}

const subscription = async () => (await navigator.serviceWorker.ready).pushManager.getSubscription()

// What the last successful report sent but its focus, so a page being put away can report without reading the subscription.
let sent: { endpoint: string | undefined; keys: { p256dh?: string; auth?: string }; origin: string } | null = null

async function send(s: PushSubscription, focused: boolean) {
  // The server refuses unknown fields, so only these are sent.
  const { endpoint, keys } = s.toJSON()
  const fields = { endpoint, keys: { p256dh: keys?.p256dh, auth: keys?.auth }, origin: location.origin }
  const { id } = await api<{ id: number }>('PUT', '/push', { ...fields, focused })
  sent = fields
  keepDevice(String(id))
}

// Called from a click: the permission is asked before anything is awaited.
export async function turnOn(key: string): Promise<void> {
  if ((await Notification.requestPermission()) !== 'granted') return
  const registration = await navigator.serviceWorker.ready
  await send(await registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key }), true)
}

export async function turnOff(): Promise<void> {
  await (await subscription())?.unsubscribe()
  const id = device()
  if (id !== null) {
    try {
      await api('DELETE', `/push/${id}`)
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 404)) throw e // a second click tries again
    }
  }
  keepDevice(null)
}

const looking = () => document.visibilityState === 'visible' && document.hasFocus()

// Every report that reads the subscription goes through one chain, so they reach the server in order.
let queue: Promise<unknown> = Promise.resolve()
const step = (f: () => Promise<unknown>) => {
  queue = queue.then(f).catch(() => {}) // the next change reports again
}

// The focus is read when the step runs, so a step that waited never reports a stale one.
const report = () =>
  step(async () => {
    const s = await subscription()
    if (s) await send(s, looking())
  })

// A page being put away may be stopped at its next await, so this report goes at once.
function away() {
  if (device() === null || sent === null) return
  const request = api('PUT', '/push', { ...sent, focused: false }, { keepalive: true }).catch(() => {})
  queue = queue.then(() => request)
}

// Tells the server whether this window has the focus, so it notifies the other devices only.
export function useFocusReport(): void {
  useEffect(() => {
    if (!available()) return
    // The server forgets focus when it restarts, so each reopened stream reports again.
    const gone = onReopen(() => device() !== null && report())
    step(async () => {
      const s = await subscription()
      if (s) await send(s, looking())
      else keepDevice(null)
    })

    const gained = () => device() !== null && report()
    const changed = () => (document.visibilityState === 'visible' ? gained() : away())
    window.addEventListener('focus', gained)
    window.addEventListener('blur', away)
    document.addEventListener('visibilitychange', changed)
    return () => {
      gone()
      window.removeEventListener('focus', gained)
      window.removeEventListener('blur', away)
      document.removeEventListener('visibilitychange', changed)
    }
  }, [])
}
