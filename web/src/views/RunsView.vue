<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import {
  connectWS,
  deleteRun,
  listRuns,
  pauseRun,
  resumeRun,
  type WSConnectionHandle
} from '../api/client'
import type { RunStatus, RunSummary } from '../api/types'
import { navigateTo } from '../router'

const runs = ref<RunSummary[]>([])
const loading = ref(false)
const errorMessage = ref('')
const statusFilter = ref<RunStatus | ''>('')
const namespaceFilter = ref('')

let wsHandle: WSConnectionHandle | null = null

async function loadRuns() {
  loading.value = true
  errorMessage.value = ''
  try {
    runs.value = await listRuns({
      namespace: namespaceFilter.value,
      status: statusFilter.value
    })
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Failed to list runs'
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void loadRuns()
  wsHandle = connectWS(undefined, {
    onEvent: (ev) => {
      if (ev.topic === 'sofa.run.events') {
        void loadRuns()
      }
    }
  })
})

onUnmounted(() => {
  wsHandle?.close()
  wsHandle = null
})

async function quickAction(runId: string, action: 'planned' | 'immediate' | 'resume' | 'delete') {
  errorMessage.value = ''
  try {
    if (action === 'planned' || action === 'immediate') {
      await pauseRun(runId, action)
    } else if (action === 'resume') {
      await resumeRun(runId)
    } else {
      await deleteRun(runId)
    }
    await loadRuns()
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Action failed'
  }
}
</script>

<template>
  <section class="runs-view">
    <div class="card row" style="justify-content: space-between">
      <div class="row">
        <h2 style="margin: 0">Workflow Runs</h2>
        <select v-model="statusFilter" @change="loadRuns">
          <option value="">All Statuses</option>
          <option value="RUNNING">RUNNING</option>
          <option value="WAITING_HUMAN">WAITING_HUMAN</option>
          <option value="PAUSING">PAUSING</option>
          <option value="PAUSED">PAUSED</option>
          <option value="SUCCEEDED">SUCCEEDED</option>
          <option value="FAILED">FAILED</option>
        </select>
        <input
          v-model="namespaceFilter"
          type="text"
          placeholder="Namespace filter"
          @keyup.enter="loadRuns"
        />
      </div>
      <button type="button" @click="loadRuns" :disabled="loading">Refresh</button>
    </div>

    <p v-if="errorMessage" class="card error-banner">{{ errorMessage }}</p>

    <div class="card">
      <div v-if="runs.length === 0 && !loading" class="muted">
        No workflow runs found. Submit one with <code>sofa apply</code>.
      </div>
      <table v-else>
        <thead>
          <tr>
            <th>Run ID</th>
            <th>Namespace</th>
            <th>Status</th>
            <th>Updated</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in runs" :key="r.run_id">
            <td class="mono">
              <a :href="`/runs/${encodeURIComponent(r.run_id)}`" @click.prevent="navigateTo(`/runs/${encodeURIComponent(r.run_id)}`)">
                {{ r.run_id }}
              </a>
            </td>
            <td><span class="badge">{{ r.namespace }}</span></td>
            <td><span class="badge" :class="`status-${r.status}`">{{ r.status }}</span></td>
            <td class="mono muted">{{ r.updated_at }}</td>
            <td>
              <div class="row">
                <button type="button" class="primary" @click="navigateTo(`/runs/${encodeURIComponent(r.run_id)}`)">
                  Inspect
                </button>
                <button type="button" class="warn" @click="quickAction(r.run_id, 'planned')">Pause</button>
                <button type="button" class="danger" @click="quickAction(r.run_id, 'immediate')">Immediate</button>
                <button type="button" @click="quickAction(r.run_id, 'resume')">Resume</button>
                <button type="button" class="danger" @click="quickAction(r.run_id, 'delete')">Delete</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<style scoped>
.runs-view {
  display: flex;
  flex-direction: column;
  gap: 1rem;
}

.error-banner {
  border-color: var(--danger);
  color: #fca5a5;
}
</style>
