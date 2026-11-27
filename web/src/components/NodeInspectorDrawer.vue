<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import {
  getNode,
  getNodeLogs,
  postDiaryMessage,
  rerunNode
} from '../api/client'
import type { LogEvent, NodeDetailResponse, PublishedArtifact } from '../api/types'
import ArtifactSandboxPreview from './ArtifactSandboxPreview.vue'

const props = defineProps<{
  runId: string
  nodeAddr: string
  liveLogs: LogEvent[]
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'mutated'): void
}>()

const detail = ref<NodeDetailResponse | null>(null)
const historicalLogs = ref<LogEvent[]>([])
const loading = ref(false)
const errorMessage = ref('')
const actionStatus = ref('')

const interjectMessage = ref('')
const waitForConfirmation = ref(false)
const rerunReason = ref('')
const previewArtifact = ref<PublishedArtifact | null>(null)

async function loadInspector() {
  if (!props.runId || !props.nodeAddr) return
  loading.value = true
  errorMessage.value = ''
  try {
    const [nodeResp, logsResp] = await Promise.all([
      getNode(props.runId, props.nodeAddr),
      getNodeLogs(props.runId, props.nodeAddr)
    ])
    detail.value = nodeResp
    historicalLogs.value = logsResp
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Failed to load node details'
  } finally {
    loading.value = false
  }
}

watch(() => [props.runId, props.nodeAddr], () => {
  previewArtifact.value = null
  actionStatus.value = ''
  loadInspector()
}, { immediate: true })

const combinedLogs = computed(() => {
  const merged = [...historicalLogs.value, ...props.liveLogs]
  const seen = new Set<string>()
  const out: LogEvent[] = []
  for (const ev of merged) {
    const key = ev.event_id || `${ev.timestamp}|${ev.epoch}|${ev.kind}|${ev.text ?? ''}|${ev.error ?? ''}`
    if (seen.has(key)) continue
    seen.add(key)
    out.push(ev)
  }
  return out
})

const effectiveBranch = computed(
  () => detail.value?.summary?.work_branch || detail.value?.outcome?.work_branch || ''
)

const effectiveCommitSHA = computed(
  () =>
    detail.value?.summary?.commit_sha ||
    detail.value?.outcome?.commit_sha ||
    detail.value?.pending_publication?.commit_sha ||
    ''
)

function isHtmlArtifact(art: PublishedArtifact): boolean {
  const lower = (art.rel_path || art.name || '').toLowerCase()
  return lower.endsWith('.html') || lower.endsWith('.htm') || art.type === 'directory'
}

async function handleRerun() {
  actionStatus.value = ''
  errorMessage.value = ''
  try {
    await rerunNode(props.runId, props.nodeAddr, rerunReason.value.trim() || undefined)
    actionStatus.value = 'Rerun requested; downstream nodes invalidated.'
    rerunReason.value = ''
    emit('mutated')
    await loadInspector()
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Rerun failed'
  }
}

async function handleInterject() {
  const msg = interjectMessage.value.trim()
  if (!msg) return
  actionStatus.value = ''
  errorMessage.value = ''
  try {
    await postDiaryMessage(props.runId, props.nodeAddr, msg, waitForConfirmation.value)
    interjectMessage.value = ''
    actionStatus.value = waitForConfirmation.value
      ? 'Interjection queued (awaiting human confirmation).'
      : 'Interjection queued.'
    await loadInspector()
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Failed to send interjection'
  }
}
</script>

<template>
  <aside class="inspector-drawer card" data-testid="node-inspector-drawer">
    <div class="drawer-header row">
      <div>
        <div class="muted">Node Inspector</div>
        <h3 class="mono drawer-title">{{ nodeAddr }}</h3>
      </div>
      <div class="row">
        <button type="button" @click="loadInspector" :disabled="loading">Refresh</button>
        <button type="button" @click="emit('close')">Close</button>
      </div>
    </div>

    <p v-if="errorMessage" class="error-text">{{ errorMessage }}</p>
    <p v-if="actionStatus" class="ok-text">{{ actionStatus }}</p>
    <p v-if="loading && !detail" class="muted">Loading node state...</p>

    <template v-if="detail">
      <section class="meta-grid">
        <div>
          <div class="muted">Status</div>
          <span class="badge" :class="`status-${detail.status}`">{{ detail.status }}</span>
        </div>
        <div>
          <div class="muted">Epoch</div>
          <span class="badge mono">e{{ detail.current_epoch }}</span>
        </div>
        <div v-if="effectiveBranch">
          <div class="muted">Work Branch</div>
          <code class="mono">{{ effectiveBranch }}</code>
        </div>
        <div v-if="effectiveCommitSHA">
          <div class="muted">Commit SHA</div>
          <code class="mono">{{ effectiveCommitSHA }}</code>
        </div>
        <div v-if="detail.worker_process_id">
          <div class="muted">Worker Process</div>
          <code class="mono">{{ detail.worker_process_id }}</code>
        </div>
      </section>

      <section class="section-block">
        <h4>Node Controls &amp; Human Interjection</h4>
        <div class="row">
          <input
            v-model="rerunReason"
            type="text"
            placeholder="Optional rerun reason"
            style="flex: 1"
          />
          <button type="button" class="warn" @click="handleRerun">Rerun Node</button>
        </div>

        <form class="interject-form" @submit.prevent="handleInterject">
          <textarea
            v-model="interjectMessage"
            rows="2"
            placeholder="Send diary interjection to active worker..."
          />
          <div class="row" style="justify-content: space-between">
            <label class="row">
              <input v-model="waitForConfirmation" type="checkbox" />
              <span>Wait for human confirmation after agent responds</span>
            </label>
            <button type="submit" class="primary" :disabled="!interjectMessage.trim()">
              Send Interjection
            </button>
          </div>
        </form>
      </section>

      <section v-if="detail.interjection_log?.length || detail.pending_interjections?.length" class="section-block">
        <h4>Interjection Timeline</h4>
        <ul class="plain-list">
          <li v-for="ij in detail.interjection_log ?? []" :key="ij.id" class="timeline-item">
            <span class="badge">{{ ij.status }}</span>
            <span>{{ ij.message }}</span>
            <span v-if="ij.wait_for_confirmation" class="badge">wait_for_confirmation</span>
          </li>
        </ul>
      </section>

      <section class="section-block">
        <h4>Checkpoints &amp; Structured Diary ({{ detail.checkpoints?.length ?? 0 }})</h4>
        <div v-if="!detail.checkpoints?.length" class="muted">No checkpoints recorded yet.</div>
        <div v-for="cp in detail.checkpoints ?? []" :key="`${cp.epoch}-${cp.seq}`" class="checkpoint-card">
          <div class="row">
            <span class="badge mono">seq #{{ cp.seq }} · e{{ cp.epoch }}</span>
            <span class="badge">{{ cp.status }}</span>
            <code class="mono muted">{{ cp.final_sha || cp.initial_sha }}</code>
          </div>
          <div v-if="cp.diary?.completed?.length" class="diary-group">
            <strong>Completed:</strong> {{ cp.diary.completed.join('; ') }}
          </div>
          <div v-if="cp.diary?.decisions?.length" class="diary-group">
            <strong>Decisions:</strong> {{ cp.diary.decisions.join('; ') }}
          </div>
          <div v-if="cp.diary?.test_evidence?.length" class="diary-group">
            <strong>Test Evidence:</strong> {{ cp.diary.test_evidence.join('; ') }}
          </div>
          <div v-if="cp.diary?.next_steps?.length" class="diary-group">
            <strong>Next Steps:</strong> {{ cp.diary.next_steps.join('; ') }}
          </div>
        </div>
      </section>

      <section class="section-block">
        <h4>Artifacts ({{ detail.artifacts?.length ?? 0 }})</h4>
        <div v-if="!detail.artifacts?.length" class="muted">No published artifacts.</div>
        <div v-for="art in detail.artifacts ?? []" :key="art.id" class="artifact-row row">
          <span class="mono">{{ art.name }} ({{ art.rel_path }})</span>
          <span class="badge mono">{{ art.id }}</span>
          <button
            v-if="isHtmlArtifact(art)"
            type="button"
            class="primary"
            @click="previewArtifact = art"
          >
            Preview Sandbox HTML
          </button>
        </div>

        <ArtifactSandboxPreview
          v-if="previewArtifact"
          :run-id="runId"
          :artifact-id="previewArtifact.id"
          :title="previewArtifact.rel_path || previewArtifact.name"
        />
      </section>

      <section class="section-block">
        <h4>Execution Logs (Live + JSONL)</h4>
        <div class="log-viewer mono" data-testid="node-log-viewer">
          <div v-if="combinedLogs.length === 0" class="muted">No log events yet.</div>
          <div v-for="(ev, idx) in combinedLogs" :key="ev.event_id || idx" class="log-line">
            <span class="muted">[e{{ ev.epoch }} {{ ev.kind }}]</span>
            <span v-if="ev.tool_name" class="tool-tag">{{ ev.tool_name }}</span>
            <span>{{ ev.text || ev.error || '' }}</span>
          </div>
        </div>
      </section>
    </template>
  </aside>
</template>

<style scoped>
.inspector-drawer {
  display: flex;
  flex-direction: column;
  gap: 1rem;
  margin-top: 1rem;
  border-color: var(--accent);
}

.drawer-header {
  justify-content: space-between;
}

.drawer-title {
  margin: 0.15rem 0 0;
}

.meta-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 0.75rem;
}

.section-block {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  border-top: 1px solid var(--border);
  padding-top: 0.75rem;
}

.section-block h4 {
  margin: 0;
  font-size: 0.92rem;
  color: var(--text-1);
}

.interject-form {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}

.plain-list {
  list-style: none;
  padding: 0;
  margin: 0;
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
}

.timeline-item {
  display: flex;
  gap: 0.5rem;
  align-items: center;
}

.checkpoint-card {
  background: #020617;
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 0.65rem;
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
  font-size: 0.85rem;
}

.artifact-row {
  justify-content: space-between;
  background: #020617;
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 0.5rem 0.75rem;
}

.log-viewer {
  background: #020617;
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 0.75rem;
  max-height: 260px;
  overflow-y: auto;
  font-size: 0.82rem;
}

.log-line {
  display: flex;
  gap: 0.45rem;
  white-space: pre-wrap;
  word-break: break-word;
}

.tool-tag {
  color: #38bdf8;
}

.error-text {
  color: #fca5a5;
  margin: 0;
}

.ok-text {
  color: #86efac;
  margin: 0;
}
</style>
