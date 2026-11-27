import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import DecisionsView from '../views/DecisionsView.vue'
import SettingsView from '../views/SettingsView.vue'
import { clearApiToken, getApiToken, setApiToken } from '../api/client'

describe('DecisionsView version-bound approvals, SettingsView write-only secrets, and sessionStorage auth', () => {
  const originalFetch = globalThis.fetch

  beforeEach(() => {
    window.sessionStorage.clear()
    window.localStorage.clear()
  })

  afterEach(() => {
    globalThis.fetch = originalFetch
    vi.restoreAllMocks()
  })

  it('stores API token strictly in sessionStorage and never in localStorage', () => {
    setApiToken('tok-session-only')
    expect(getApiToken()).toBe('tok-session-only')
    expect(window.sessionStorage.getItem('sofa_api_token')).toBe('tok-session-only')
    expect(window.localStorage.length).toBe(0)

    clearApiToken()
    expect(getApiToken()).toBe('')
    expect(window.sessionStorage.getItem('sofa_api_token')).toBeNull()
  })

  it('binds exact (commit_sha, artifact_ids) on human_approval Approve and Return Upstream Fallback', async () => {
    const pendingCards = [
      {
        card_id: 'dec-approve-1',
        run_id: 'run-9',
        node_addr: 'worker.impl',
        epoch: 3,
        kind: 'human_approval',
        status: 'PENDING',
        pause_reason: 'Requires human sign-off before merge',
        commit_sha: 'deadbeef1234567890abcdef1234567890abcdef',
        artifact_ids: ['art-1', 'art-2'],
        created_at: '2026-11-27T10:00:00Z',
        updated_at: '2026-11-27T10:00:00Z'
      }
    ]

    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.startsWith('/api/v1/decisions?status=PENDING')) {
        return {
          ok: true,
          status: 200,
          json: async () => ({ decisions: pendingCards })
        } as Response
      }
      if (url === '/api/v1/decisions/dec-approve-1/respond' && init?.method === 'POST') {
        return {
          ok: true,
          status: 200,
          json: async () => ({ accepted: true, status: 'SUCCEEDED', auto_resumed: true, epoch: 3 })
        } as Response
      }
      return { ok: true, status: 200, json: async () => ({ decisions: [] }) } as Response
    })
    globalThis.fetch = fetchMock as unknown as typeof fetch

    const wrapper = mount(DecisionsView)
    await flushPromises()

    expect(wrapper.text()).toContain('deadbeef1234567890abcdef1234567890abcdef')
    expect(wrapper.text()).toContain('art-1')
    expect(wrapper.text()).toContain('art-2')

    // Click Approve
    const approveBtn = wrapper.find('[data-testid="approve-dec-approve-1"]')
    expect(approveBtn.exists()).toBe(true)
    await approveBtn.trigger('click')
    await flushPromises()

    const postCall = fetchMock.mock.calls.find(
      ([u, i]) => String(u) === '/api/v1/decisions/dec-approve-1/respond' && i?.method === 'POST'
    )
    expect(postCall).toBeDefined()
    const payload = JSON.parse(String(postCall![1]!.body))
    expect(payload).toEqual({
      run_id: 'run-9',
      node_addr: 'worker.impl',
      approved: true,
      commit_sha: 'deadbeef1234567890abcdef1234567890abcdef',
      artifact_ids: ['art-1', 'art-2'],
      answer_text: ''
    })

    // Also test Return Upstream Fallback
    const wrapper2 = mount(DecisionsView)
    await flushPromises()
    const fallbackInput = wrapper2.find('[data-testid="fallback-input-dec-approve-1"]')
    await fallbackInput.setValue('re_architect')
    const returnBtn = wrapper2.find('[data-testid="return-upstream-dec-approve-1"]')
    await returnBtn.trigger('click')
    await flushPromises()

    const postCalls = fetchMock.mock.calls.filter(
      ([u, i]) => String(u) === '/api/v1/decisions/dec-approve-1/respond' && i?.method === 'POST'
    )
    expect(postCalls.length).toBe(2)
    const fallbackPayload = JSON.parse(String(postCalls[1][1]!.body))
    expect(fallbackPayload).toEqual({
      run_id: 'run-9',
      node_addr: 'worker.impl',
      approved: false,
      returned_upstream: true,
      return_fallback: 're_architect',
      commit_sha: 'deadbeef1234567890abcdef1234567890abcdef',
      artifact_ids: ['art-1', 'art-2'],
      answer_text: ''
    })
  })

  it('renders SettingsView secret input with type="password" and lists write-only secret metadata without plaintext', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
      const url = String(input)
      if (url.startsWith('/api/v1/envs')) {
        return {
          ok: true,
          status: 200,
          json: async () => ({
            scope: 'global',
            envs: [
              {
                name: 'LOG_LEVEL',
                value: 'info',
                scope: 'global',
                updated_at: '2026-11-27T08:00:00Z'
              }
            ]
          })
        } as Response
      }
      if (url.startsWith('/api/v1/secrets')) {
        return {
          ok: true,
          status: 200,
          json: async () => ({
            scope: 'global',
            secrets: [
              {
                name: 'GITHUB_TOKEN',
                scope: 'global',
                updated_at: '2026-11-27T09:00:00Z'
              }
            ]
          })
        } as Response
      }
      return { ok: true, status: 200, json: async () => ({}) } as Response
    })
    globalThis.fetch = fetchMock as unknown as typeof fetch

    const wrapper = mount(SettingsView)
    await flushPromises()

    const secretInput = wrapper.find('[data-testid="secret-value-input"]')
    expect(secretInput.exists()).toBe(true)
    expect(secretInput.attributes('type')).toBe('password')

    const secretsTable = wrapper.find('[data-testid="secrets-table"]')
    expect(secretsTable.exists()).toBe(true)
    expect(secretsTable.text()).toContain('GITHUB_TOKEN')
    expect(secretsTable.text()).toContain('2026-11-27T09:00:00Z')
    expect(secretsTable.text()).toContain('write-only')

    // Store / Rotate secret sends PUT /api/v1/secrets without exposing value in table
    const nameInput = wrapper.find('input[placeholder="SECRET_NAME"]')
    await nameInput.setValue('API_KEY')
    await secretInput.setValue('super-secret-value')
    const secretForm = secretInput.element.closest('form')
    expect(secretForm).not.toBeNull()
    await wrapper.findAll('form')[1].trigger('submit')
    await flushPromises()

    const putSecretCall = fetchMock.mock.calls.find(
      ([u, i]) => String(u) === '/api/v1/secrets' && (i as RequestInit)?.method === 'PUT'
    )
    expect(putSecretCall).toBeDefined()
    expect(JSON.parse(String(putSecretCall![1]!.body))).toEqual({
      scope: 'global',
      name: 'API_KEY',
      value: 'super-secret-value'
    })
  })
})
