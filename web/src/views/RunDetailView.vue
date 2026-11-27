<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import {
  connectWS,
  deleteRun,
  getRun,
  pauseRun,
  resumeRun,
  type WSConnectionHandle
} from '../api/client'
import type { LogEvent, NodeStatus, RunDetailResponse, RunStatus, WSEvent } from '../api/types'
import DagGraph from '../components/DagGraph.vue'
import NodeInspectorDrawer from '../components/NodeInspectorDrawer.vue'

const props = defineProps<{
  runId: string
}>()

const emit = defineEmits<{
  (e: 'deleted'): void
  (e: 'back'): void
}>()

const run = ref<RunDetailResponse | null>(null)
const loading = ref(false)
const errorMessage = ref('')
const revealSensitive = ref(false)
const selectedNodeAddr = ref('')
const wsStatus = ref<'connecting' | 'open' | 'closed' | 'error'>('closed')
const liveLogsByNode = ref<Record<string, LogEvent[]>>({})

let wsHandle: WSConnectionHandle | null = null

async function loadRun() {
  if (!props.runId) return
  loading.value = true
  errorMessage.value = ''
  try {
    run.value = await getRun(props.runId, revealSensitive.value)
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Failed to load run'
  } finally {
    loading.value = false
  }
}

function handleWSEvent(ev: WSEvent) {
  const payload = (ev.payload ?? {}) as Record<string, unknown>
  if (ev.topic === 'sofa-events') {
    const nodeAddr = String(ev.node_addr || payload.node_addr || '')
    if (!nodeAddr) return
    const logEv: LogEvent = {
      timestamp: String(payload.timestamp || ev.timestamp || new Date().toISOString()),
      run_id: String(payload.run_id || ev.run_id || props.runId),
      node_addr: nodeAddr,
      epoch: Number(payload.epoch ?? ev.epoch ?? 0),
      process_id: String(payload.process_id || ''),
      event_id: payload.event_id ? String(payload.event_id) : undefined,
      kind: String(payload.kind || ev.type || 'log'),
      tool_call_id: payload.tool_call_id ? String(payload.tool_call_id) : undefined,
      tool_name: payload.tool_name ? String(payload.tool_name) : undefined,
      text: payload.text !== undefined ? String(payload.text) : undefined,
      error: payload.error !== undefined ? String(payload.error) : undefined
    }
    const existing = liveLogsByNode.value[nodeAddr] ?? []
    liveLogsByNode.value = {
      ...liveLogsByNode.value,
      [nodeAddr]: [...existing, logEv]
    }
    return
  }

  if (ev.topic === 'sofa.run.events' && run.value) {
    const nodeAddr = String(ev.node_addr || payload.node_addr || '')
    const nextStatus = payload.status ? String(payload.status) : ''
    const nextEpoch = Number(payload.epoch ?? ev.epoch ?? 0)
    if (nodeAddr && run.value.nodes) {
      let updated = false
      run.value = {
        ...run.value,
        nodes: run.value.nodes.map((n) => {
          if (n.canonical_address === nodeAddr || n.address === nodeAddr) {
            updated = true
            return {
              ...n,
              status: (nextStatus || n.status) as NodeStatus,
              current_epoch: nextEpoch > 0 ? nextEpoch : n.current_epoch
            }
          }
          return n
        })
      }
      if (!updated) {
        void loadRun()
      }
    } else if (nextStatus) {
      run.value = {
        ...run.value,
        status: nextStatus as RunStatus
      }
    }
  }
}

function reconnectWS() {
  wsHandle?.close()
  wsHandle = connectWS(props.runId, {
    onEvent: handleWSEvent,
    onStatusChange: (st) => {
      wsStatus.value = st
    }
  })
}

watch(
  () => props.runId,
  () => {
    selectedNodeAddr.value = ''
    liveLogsByNode.value = {}
    void loadRun()
    reconnectWS()
  }
)

onMounted(() => {
  void loadRun()
  reconnectWS()
})

onUnmounted(() => {
  wsHandle?.close()
  wsHandle = null
})

async function handlePause(mode: 'planned' | 'immediate') {
  errorMessage.value = ''
  try {
    await pauseRun(props.runId, mode)
    await loadRun()
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Pause failed'
  }
}

async function handleResume() {
  errorMessage.value = ''
  try {
    await resumeRun(props.runId)
    await loadRun()
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Resume failed'
  }
}

async function handleDelete() {
  errorMessage.value = ''
  try {
    await deleteRun(props.runId)
    emit('deleted')
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Delete failed'
  }
}

function toggleSensitive() {
  revealSensitive.value = !revealSensitive.value
  void loadRun()
}

const outputEntries = computed(() => Object.values(run.value?.outputs ?? {}))
const selectedNodeLiveLogs = computed(() =>
  selectedNodeAddr.value ? liveLogsByNode.value[selectedNodeAddr.value] ?? [] : []
)
</script>

<template>
  <section class="run-detail">
    <div class="card row" style="justify-content: space-between">
      <div class="row">
        <button type="button" @click="emit('back')">← All Runs</button>
        <h2 class="mono" style="margin: 0">{{ runId }}</h2>
        <span v-if="run" class="badge">{{ run.namespace }}</span>
        <span v-if="run" class="badge" :class="`status-${run.status}`">{{ run.status }}</span>
        <span class="badge mono">WS: {{ wsStatus }}</span>
      </div>

      <div class="row">
        <button type="button" @click="loadRun" :disabled="loading">Refresh</button>
        <button type="button" class="warn" @click="handlePause('planned')">Pause (Planned)</button>
        <button type="button" class="danger" @click="handlePause('immediate')">Pause (Immediate)</button>
        <button type="button" class="primary" @click="handleResume">Resume</button>
        <button type="button" class="danger" @click="handleDelete">Delete</button>
      </div>
    </div>

    <p v-if="errorMessage" class="error-banner card">{{ errorMessage }}</p>

    <div class="card">
      <div class="row" style="justify-content: space-between; margin-bottom: 0.75rem">
        <h3 style="margin: 0">DAG Topology</h3>
        <span class="muted">Click any node to inspect epoch checkpoints, diary, logs, and artifacts</span>
      </div>
      <DagGraph
        :nodes="run?.nodes ?? []"
        :selected-node-addr="selectedNodeAddr"
        @select="(addr) => (selectedNodeAddr = addr)"
      />
    </div>

    <NodeInspectorDrawer
      v-if="selectedNodeAddr"
      :run-id="runId"
      :node-addr="selectedNodeAddr"
      :live-logs="selectedNodeLiveLogs"
      @close="selectedNodeAddr = ''"
      @mutated="loadRun"
    />

    <div class="card">
      <div class="row" style="justify-content: space-between; margin-bottom: 0.75rem">
        <h3 style="margin: 0">Run Outputs</h3>
        <button type="button" @click="toggleSensitive">
          {{ revealSensitive ? 'Mask Sensitive' : 'Reveal Sensitive' }}
        </button>
      </div>
      <div v-if="outputEntries.length === 0" class="muted">No outputs evaluated yet.</div>
      <table v-else>
        <thead>
          <tr>
            <th>Name</th>
            <th>Sensitive</th>
            <th>Value</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="out in outputEntries" :key="out.name">
            <td class="mono">{{ out.name }}</td>
            <td><span class="badge">{{ out.sensitive ? 'sensitive' : 'plain' }}</span></td>
            <td class="mono">
              {{ out.unknown ? '(unknown)' : JSON.stringify(out.value) }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<style scoped>
.run-detail {
  display: flex;
  flex-direction: column;
  gap: 1rem;
}

.error-banner {
  border-color: var(--danger);
  color: #fca5a5;
}
</style>
