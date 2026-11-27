import type {
  ConfigScope,
  DecisionCard,
  DecisionCardStatus,
  EnvEntry,
  LogEvent,
  NodeDetailResponse,
  RespondDecisionPayload,
  RunDetailResponse,
  RunStatus,
  RunSummary,
  SecretMeta,
  WSEvent
} from './types'

const TOKEN_STORAGE_KEY = 'sofa_api_token'

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

export function getApiToken(): string {
  try {
    return window.sessionStorage.getItem(TOKEN_STORAGE_KEY) ?? ''
  } catch {
    return ''
  }
}

export function setApiToken(token: string): void {
  const trimmed = token.trim()
  if (!trimmed) {
    clearApiToken()
    return
  }
  window.sessionStorage.setItem(TOKEN_STORAGE_KEY, trimmed)
}

export function clearApiToken(): void {
  window.sessionStorage.removeItem(TOKEN_STORAGE_KEY)
}

export function base64UrlEncode(input: string): string {
  const bytes = new TextEncoder().encode(input)
  let binary = ''
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i])
  }
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '')
}

async function apiRequest(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers ?? {})
  const token = getApiToken()
  if (token) {
    headers.set('Authorization', `Bearer ${token}`)
  }
  if (init.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  const resp = await fetch(path, {
    ...init,
    headers
  })
  if (!resp.ok) {
    let msg = `HTTP ${resp.status}`
    try {
      const parsed = (await resp.json()) as { error?: string }
      if (parsed?.error) {
        msg = parsed.error
      }
    } catch {
      // ignore parse failure
    }
    throw new ApiError(resp.status, msg)
  }
  return resp
}

async function apiJSON<T>(path: string, init: RequestInit = {}): Promise<T> {
  const resp = await apiRequest(path, init)
  return (await resp.json()) as T
}

export async function listRuns(params?: {
  namespace?: string
  status?: RunStatus | ''
}): Promise<RunSummary[]> {
  const q = new URLSearchParams()
  if (params?.namespace?.trim()) {
    q.set('namespace', params.namespace.trim())
  }
  if (params?.status) {
    q.set('status', params.status)
  }
  const qs = q.toString()
  const data = await apiJSON<{ runs: RunSummary[] }>(`/api/v1/runs${qs ? `?${qs}` : ''}`)
  return data.runs ?? []
}

export async function getRun(runId: string, revealSensitive = false): Promise<RunDetailResponse> {
  const q = revealSensitive ? '?reveal_sensitive=true' : ''
  return apiJSON<RunDetailResponse>(`/api/v1/runs/${encodeURIComponent(runId)}${q}`)
}

export async function pauseRun(runId: string, mode: 'planned' | 'immediate'): Promise<void> {
  await apiJSON(`/api/v1/runs/${encodeURIComponent(runId)}/pause?mode=${mode}`, {
    method: 'POST',
    body: JSON.stringify({ mode })
  })
}

export async function resumeRun(runId: string): Promise<void> {
  await apiJSON(`/api/v1/runs/${encodeURIComponent(runId)}/resume`, {
    method: 'POST'
  })
}

export async function deleteRun(runId: string): Promise<void> {
  await apiJSON(`/api/v1/runs/${encodeURIComponent(runId)}`, {
    method: 'DELETE'
  })
}

export async function getNode(runId: string, nodeAddr: string): Promise<NodeDetailResponse> {
  return apiJSON<NodeDetailResponse>(
    `/api/v1/runs/${encodeURIComponent(runId)}/nodes/${encodeURIComponent(nodeAddr)}`
  )
}

export async function getNodeLogs(
  runId: string,
  nodeAddr: string,
  epoch?: number
): Promise<LogEvent[]> {
  const q = epoch && epoch > 0 ? `?epoch=${epoch}` : ''
  try {
    const resp = await apiRequest(
      `/api/v1/runs/${encodeURIComponent(runId)}/nodes/${encodeURIComponent(nodeAddr)}/logs${q}`
    )
    const raw = await resp.text()
    const events: LogEvent[] = []
    for (const line of raw.split('\n')) {
      const trimmed = line.trim()
      if (!trimmed) continue
      try {
        events.push(JSON.parse(trimmed) as LogEvent)
      } catch {
        // skip malformed line
      }
    }
    return events
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      return []
    }
    throw err
  }
}

export async function rerunNode(runId: string, nodeAddr: string, reason?: string): Promise<void> {
  await apiJSON(
    `/api/v1/runs/${encodeURIComponent(runId)}/nodes/${encodeURIComponent(nodeAddr)}/rerun`,
    {
      method: 'POST',
      body: JSON.stringify(reason ? { reason } : {})
    }
  )
}

export async function postDiaryMessage(
  runId: string,
  nodeAddr: string,
  message: string,
  waitForConfirmation: boolean
): Promise<void> {
  await apiJSON(
    `/api/v1/runs/${encodeURIComponent(runId)}/nodes/${encodeURIComponent(nodeAddr)}/diary-messages`,
    {
      method: 'POST',
      body: JSON.stringify({
        message,
        wait_for_confirmation: waitForConfirmation
      })
    }
  )
}

export async function mintPreviewUrl(runId: string, artifactId: string): Promise<string> {
  const data = await apiJSON<{ ticket?: string; preview_url?: string }>(
    `/api/v1/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(artifactId)}/preview-ticket`,
    { method: 'POST' }
  )
  if (data.preview_url) {
    return data.preview_url
  }
  if (!data.ticket) {
    throw new Error('Server did not return a preview ticket')
  }
  return `/api/v1/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(artifactId)}/content?ticket=${encodeURIComponent(data.ticket)}`
}

export async function listDecisions(params?: {
  run_id?: string
  status?: DecisionCardStatus | ''
}): Promise<DecisionCard[]> {
  const q = new URLSearchParams()
  if (params?.run_id?.trim()) {
    q.set('run_id', params.run_id.trim())
  }
  if (params?.status) {
    q.set('status', params.status)
  }
  const qs = q.toString()
  const data = await apiJSON<{ decisions: DecisionCard[] }>(
    `/api/v1/decisions${qs ? `?${qs}` : ''}`
  )
  return data.decisions ?? []
}

export async function respondDecision(
  cardId: string,
  payload: RespondDecisionPayload
): Promise<void> {
  await apiJSON(`/api/v1/decisions/${encodeURIComponent(cardId)}/respond`, {
    method: 'POST',
    body: JSON.stringify(payload)
  })
}

function buildScopeQuery(scope: ConfigScope, namespace?: string): string {
  const q = new URLSearchParams()
  q.set('scope', scope)
  if (scope === 'namespace') {
    q.set('namespace', namespace?.trim() || 'default')
  }
  return q.toString()
}

export async function listEnvs(scope: ConfigScope, namespace?: string): Promise<EnvEntry[]> {
  const data = await apiJSON<{ envs: EnvEntry[] }>(
    `/api/v1/envs?${buildScopeQuery(scope, namespace)}`
  )
  return data.envs ?? []
}

export async function upsertEnv(
  scope: ConfigScope,
  namespace: string | undefined,
  name: string,
  value: string
): Promise<void> {
  await apiJSON('/api/v1/envs', {
    method: 'PUT',
    body: JSON.stringify({
      scope,
      namespace: scope === 'namespace' ? namespace?.trim() || 'default' : undefined,
      name,
      value
    })
  })
}

export async function deleteEnv(
  scope: ConfigScope,
  namespace: string | undefined,
  name: string
): Promise<void> {
  await apiJSON(`/api/v1/envs/${encodeURIComponent(name)}?${buildScopeQuery(scope, namespace)}`, {
    method: 'DELETE'
  })
}

export async function listSecrets(scope: ConfigScope, namespace?: string): Promise<SecretMeta[]> {
  const data = await apiJSON<{ secrets: SecretMeta[] }>(
    `/api/v1/secrets?${buildScopeQuery(scope, namespace)}`
  )
  return data.secrets ?? []
}

export async function upsertSecret(
  scope: ConfigScope,
  namespace: string | undefined,
  name: string,
  value: string
): Promise<void> {
  await apiJSON('/api/v1/secrets', {
    method: 'PUT',
    body: JSON.stringify({
      scope,
      namespace: scope === 'namespace' ? namespace?.trim() || 'default' : undefined,
      name,
      value
    })
  })
}

export async function deleteSecret(
  scope: ConfigScope,
  namespace: string | undefined,
  name: string
): Promise<void> {
  await apiJSON(
    `/api/v1/secrets/${encodeURIComponent(name)}?${buildScopeQuery(scope, namespace)}`,
    { method: 'DELETE' }
  )
}

export interface WSConnectionHandle {
  close: () => void
}

export function buildWSProtocols(token: string): string[] {
  const trimmed = token.trim()
  if (!trimmed) {
    return ['sofa.v1']
  }
  return ['sofa.v1', `sofa.bearer.${base64UrlEncode(trimmed)}`]
}

export function connectWS(
  runId: string | undefined,
  callbacks: {
    onEvent: (event: WSEvent) => void
    onStatusChange?: (status: 'connecting' | 'open' | 'closed' | 'error') => void
  }
): WSConnectionHandle {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const host = window.location.host || '127.0.0.1:8080'
  const q = runId ? `?run_id=${encodeURIComponent(runId)}` : ''
  const url = `${proto}//${host}/api/v1/ws${q}`
  const protocols = buildWSProtocols(getApiToken())

  callbacks.onStatusChange?.('connecting')
  const ws = new WebSocket(url, protocols)

  ws.onopen = () => {
    callbacks.onStatusChange?.('open')
  }
  ws.onerror = () => {
    callbacks.onStatusChange?.('error')
  }
  ws.onclose = () => {
    callbacks.onStatusChange?.('closed')
  }
  ws.onmessage = (msg: MessageEvent) => {
    try {
      const parsed = JSON.parse(String(msg.data)) as WSEvent
      callbacks.onEvent(parsed)
    } catch {
      // ignore non-JSON frame
    }
  }

  return {
    close: () => {
      try {
        ws.close()
      } catch {
        // ignore close error
      }
    }
  }
}
