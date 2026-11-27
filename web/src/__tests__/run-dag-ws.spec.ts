import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import RunDetailView from '../views/RunDetailView.vue'
import { setApiToken } from '../api/client'

class MockWebSocket {
  static instances: MockWebSocket[] = []
  url: string
  protocols: string | string[] | undefined
  onopen: ((ev: Event) => void) | null = null
  onmessage: ((ev: MessageEvent) => void) | null = null
  onerror: ((ev: Event) => void) | null = null
  onclose: ((ev: CloseEvent) => void) | null = null
  close = vi.fn()

  constructor(url: string, protocols?: string | string[]) {
    this.url = url
    this.protocols = protocols
    MockWebSocket.instances.push(this)
  }

  emitOpen() {
    this.onopen?.(new Event('open'))
  }

  emitMessage(data: unknown) {
    this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(data) }))
  }
}

describe('RunDetailView live WebSocket DAG & incremental log stream', () => {
  const originalFetch = globalThis.fetch
  const originalWS = globalThis.WebSocket

  beforeEach(() => {
    MockWebSocket.instances = []
    window.sessionStorage.clear()
    window.localStorage.clear()
    globalThis.WebSocket = MockWebSocket as unknown as typeof WebSocket
  })

  afterEach(() => {
    globalThis.fetch = originalFetch
    globalThis.WebSocket = originalWS
    vi.restoreAllMocks()
  })

  it('connects WS with sofa.v1 + sofa.bearer.<base64url>, updates node status/epoch, and appends live sofa-events logs', async () => {
    setApiToken('my-secret-token')

    const fetchMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
      const url = String(input)
      if (url.startsWith('/api/v1/runs/run-42/nodes/worker.plan/logs')) {
        return {
          ok: true,
          status: 200,
          text: async () =>
            JSON.stringify({
              timestamp: '2026-11-27T10:00:00Z',
              run_id: 'run-42',
              node_addr: 'worker.plan',
              epoch: 1,
              process_id: 'proc-1',
              kind: 'assistant_text',
              text: 'historical log line'
            }) + '\n'
        } as Response
      }
      if (url === '/api/v1/runs/run-42/nodes/worker.plan') {
        return {
          ok: true,
          status: 200,
          json: async () => ({
            run_id: 'run-42',
            node_addr: 'worker.plan',
            current_epoch: 2,
            status: 'RUNNING',
            manual_paused: false,
            summary: {
              address: 'worker.plan',
              canonical_address: 'worker.plan',
              kind: 'worker',
              status: 'RUNNING',
              current_epoch: 2,
              work_branch: 'sofa/work/run-42/worker.plan',
              commit_sha: 'abc1234'
            },
            checkpoints: [
              {
                seq: 1,
                epoch: 1,
                branch: 'sofa/ckpt/run-42/worker.plan/e1',
                initial_sha: '0000',
                final_sha: '1111',
                status: 'COMPLETE',
                diary: {
                  completed: ['planned steps'],
                  next_steps: ['implement']
                }
              }
            ],
            artifacts: []
          })
        } as Response
      }
      if (url.startsWith('/api/v1/runs/run-42')) {
        return {
          ok: true,
          status: 200,
          json: async () => ({
            run_id: 'run-42',
            namespace: 'default',
            status: 'RUNNING',
            manual_paused: false,
            created_at: '2026-11-27T09:59:00Z',
            updated_at: '2026-11-27T10:00:00Z',
            nodes: [
              {
                address: 'worker.plan',
                canonical_address: 'worker.plan',
                kind: 'worker',
                status: 'RUNNING',
                current_epoch: 1
              },
              {
                address: 'merge.main',
                canonical_address: 'merge.main',
                kind: 'merge',
                status: 'PENDING',
                current_epoch: 0,
                depends_on: ['worker.plan']
              }
            ]
          })
        } as Response
      }
      return { ok: true, status: 200, json: async () => ({}) } as Response
    })
    globalThis.fetch = fetchMock as unknown as typeof fetch

    const wrapper = mount(RunDetailView, {
      props: { runId: 'run-42' }
    })

    await flushPromises()

    expect(MockWebSocket.instances.length).toBe(1)
    const ws = MockWebSocket.instances[0]
    expect(ws.url).toContain('/api/v1/ws?run_id=run-42')
    // base64url("my-secret-token") without '=' padding is "bXktc2VjcmV0LXRva2Vu"
    expect(ws.protocols).toEqual(['sofa.v1', 'sofa.bearer.bXktc2VjcmV0LXRva2Vu'])

    ws.emitOpen()
    ws.emitMessage({
      topic: 'sofa.run.events',
      type: 'node_status',
      run_id: 'run-42',
      node_addr: 'worker.plan',
      epoch: 2,
      timestamp: '2026-11-27T10:00:03Z',
      payload: {
        run_id: 'run-42',
        node_addr: 'worker.plan',
        epoch: 2,
        status: 'SUCCEEDED'
      }
    })
    await flushPromises()

    const nodeCard = wrapper.find('[data-node-addr="worker.plan"]')
    expect(nodeCard.exists()).toBe(true)
    expect(nodeCard.text()).toContain('SUCCEEDED')
    expect(nodeCard.text()).toContain('e2')

    // Open Node Inspector drawer
    await nodeCard.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('historical log line')

    // Stream incremental sofa-events log frame
    ws.emitMessage({
      topic: 'sofa-events',
      type: 'assistant_text',
      run_id: 'run-42',
      node_addr: 'worker.plan',
      epoch: 2,
      timestamp: '2026-11-27T10:00:05Z',
      payload: {
        timestamp: '2026-11-27T10:00:05Z',
        run_id: 'run-42',
        node_addr: 'worker.plan',
        epoch: 2,
        process_id: 'proc-1',
        kind: 'assistant_text',
        text: 'live streamed line'
      }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('live streamed line')

    // Send human interjection with wait_for_confirmation=true
    const textarea = wrapper.find('textarea')
    await textarea.setValue('focus on edge cases')
    const waitCheckbox = wrapper.find('input[type="checkbox"]')
    await waitCheckbox.setValue(true)
    await wrapper.find('form.interject-form').trigger('submit')
    await flushPromises()

    const interjectCall = fetchMock.mock.calls.find(
      ([u]) => String(u) === '/api/v1/runs/run-42/nodes/worker.plan/diary-messages'
    )
    expect(interjectCall).toBeDefined()
    expect(JSON.parse(String(interjectCall![1]!.body))).toEqual({
      message: 'focus on edge cases',
      wait_for_confirmation: true
    })

    // Toggle Reveal Sensitive outputs
    const revealBtn = wrapper.findAll('button').find((b) => b.text().includes('Reveal Sensitive'))
    expect(revealBtn).toBeDefined()
    await revealBtn!.trigger('click')
    await flushPromises()
    expect(
      fetchMock.mock.calls.some(([u]) => String(u) === '/api/v1/runs/run-42?reveal_sensitive=true')
    ).toBe(true)
  })
})
