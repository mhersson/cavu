import '@testing-library/jest-dom/vitest'

// jsdom has no matchMedia; a test that needs one replaces it.
if (typeof window.matchMedia !== 'function') {
  window.matchMedia = (query: string) =>
    ({
      matches: false,
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
    }) as unknown as MediaQueryList
}

// jsdom has no ResizeObserver and lays nothing out, so nothing it observes ever resizes.
if (typeof globalThis.ResizeObserver !== 'function') {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
}

// Node 25 and later expose no localStorage by default and jsdom does not install one there.
if (typeof globalThis.localStorage?.getItem !== 'function') {
  const items = new Map<string, string>()
  const storage: Storage = {
    get length() {
      return items.size
    },
    clear: () => items.clear(),
    getItem: (k) => items.get(k) ?? null,
    key: (i) => [...items.keys()][i] ?? null,
    removeItem: (k) => void items.delete(k),
    setItem: (k, v) => void items.set(k, String(v)),
  }
  Object.defineProperty(globalThis, 'localStorage', { value: storage, configurable: true })
  Object.defineProperty(window, 'localStorage', { value: storage, configurable: true })
}

// What a push fake put on navigator.
afterEach(() => {
  localStorage.clear()
  Reflect.deleteProperty(navigator, 'serviceWorker')
  Reflect.deleteProperty(navigator, 'standalone')
})
