import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import ArtifactSandboxPreview from '../components/ArtifactSandboxPreview.vue'

describe('ArtifactSandboxPreview zero-trust sandbox red-line', () => {
  const originalFetch = globalThis.fetch

  beforeEach(() => {
    window.sessionStorage.clear()
    window.localStorage.clear()
  })

  afterEach(() => {
    globalThis.fetch = originalFetch
    vi.restoreAllMocks()
  })

  it('mints scoped preview ticket and renders iframe strictly with sandbox="allow-scripts" and no allow-same-origin', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({
        ticket: 'tkt-signed-123',
        expires_at: '2026-11-27T12:00:00Z'
      })
    })
    globalThis.fetch = fetchMock as unknown as typeof fetch

    const wrapper = mount(ArtifactSandboxPreview, {
      props: {
        runId: 'run-1',
        artifactId: 'art-html-1',
        title: 'prototype.html'
      }
    })

    await flushPromises()

    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/v1/runs/run-1/artifacts/art-html-1/preview-ticket')
    expect((init as RequestInit)?.method).toBe('POST')

    const iframe = wrapper.find('iframe')
    expect(iframe.exists()).toBe(true)

    const sandboxAttr = iframe.attributes('sandbox')
    expect(sandboxAttr).toBe('allow-scripts')
    expect(sandboxAttr).not.toContain('allow-same-origin')
    expect(sandboxAttr).not.toContain('allow-top-navigation')
    expect(sandboxAttr).not.toContain('allow-popups')
    expect(sandboxAttr).not.toContain('allow-forms')

    expect(iframe.attributes('referrerpolicy')).toBe('no-referrer')
    expect(iframe.attributes('src')).toBe(
      '/api/v1/runs/run-1/artifacts/art-html-1/content?ticket=tkt-signed-123'
    )
  })
})
