import { useEffect } from 'react'
import { api, ApiError } from './api/client.ts'
import { device, keepDevice } from './api/stream.ts'

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

async function send(s: PushSubscription, focused: boolean) {
  // The server refuses unknown fields, so only these are sent.
  const { endpoint, keys } = s.toJSON()
  const body = { endpoint, keys: { p256dh: keys?.p256dh, auth: keys?.auth }, origin: location.origin, focused }
  const { id } = await api<{ id: number }>('PUT', '/push', body)
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

// Tells the server whether this window has the focus, so it notifies the other devices only.
export function useFocusReport(): void {
  useEffect(() => {
    if (!available()) return
    const report = (focused: boolean) => {
      if (device() === null) return
      subscription()
        .then((s) => s && send(s, focused))
        .catch(() => {}) // the next change reports again
    }
    subscription()
      .then((s) => (s ? send(s, looking()) : keepDevice(null)))
      .catch(() => {})

    const gained = () => report(true)
    const lost = () => report(false)
    const changed = () => report(looking())
    window.addEventListener('focus', gained)
    window.addEventListener('blur', lost)
    document.addEventListener('visibilitychange', changed)
    return () => {
      window.removeEventListener('focus', gained)
      window.removeEventListener('blur', lost)
      document.removeEventListener('visibilitychange', changed)
    }
  }, [])
}
