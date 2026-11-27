function createMemoryStorage(): Storage {
  const store = new Map<string, string>()
  return {
    get length() {
      return store.size
    },
    clear() {
      store.clear()
    },
    getItem(key: string) {
      return store.has(key) ? store.get(key)! : null
    },
    key(index: number) {
      return Array.from(store.keys())[index] ?? null
    },
    removeItem(key: string) {
      store.delete(key)
    },
    setItem(key: string, value: string) {
      store.set(String(key), String(value))
    }
  }
}

if (typeof window !== 'undefined') {
  if (!window.localStorage) {
    Object.defineProperty(window, 'localStorage', {
      value: createMemoryStorage(),
      configurable: true
    })
  }
  if (!window.sessionStorage) {
    Object.defineProperty(window, 'sessionStorage', {
      value: createMemoryStorage(),
      configurable: true
    })
  }
}
