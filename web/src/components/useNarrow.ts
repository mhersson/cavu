import { useSyncExternalStore } from 'react'

const query = '(max-width: 720px)'

function subscribe(onChange: () => void) {
  const mql = window.matchMedia(query)
  mql.addEventListener('change', onChange)
  return () => mql.removeEventListener('change', onChange)
}

// True on a phone: the frame then has four tabs and no navigation along the top.
export function useNarrow(): boolean {
  return useSyncExternalStore(subscribe, () => window.matchMedia(query).matches)
}
