import { computed, ref } from 'vue'

export type RouteName = 'runs' | 'run-detail' | 'decisions' | 'settings'

export interface ParsedRoute {
  name: RouteName
  runId?: string
}

export function parsePath(pathname: string): ParsedRoute {
  const clean = pathname.replace(/\/+$/, '') || '/'
  if (clean === '/' || clean === '/runs') {
    return { name: 'runs' }
  }
  if (clean.startsWith('/runs/')) {
    const runId = decodeURIComponent(clean.slice('/runs/'.length))
    if (runId) {
      return { name: 'run-detail', runId }
    }
    return { name: 'runs' }
  }
  if (clean === '/decisions') {
    return { name: 'decisions' }
  }
  if (clean === '/settings') {
    return { name: 'settings' }
  }
  return { name: 'runs' }
}

const currentPath = ref(typeof window !== 'undefined' ? window.location.pathname : '/')

if (typeof window !== 'undefined') {
  window.addEventListener('popstate', () => {
    currentPath.value = window.location.pathname
  })
}

export const currentRoute = computed(() => parsePath(currentPath.value))

export function navigateTo(path: string): void {
  if (typeof window !== 'undefined' && window.location.pathname !== path) {
    window.history.pushState({}, '', path)
  }
  currentPath.value = path
}
