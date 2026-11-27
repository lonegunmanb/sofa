<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import {
  connectWS,
  listDecisions,
  respondDecision,
  type WSConnectionHandle
} from '../api/client'
import type { DecisionCard, DecisionCardStatus, RespondDecisionPayload } from '../api/types'
import DecisionCardItem from '../components/DecisionCardItem.vue'

const cards = ref<DecisionCard[]>([])
const statusFilter = ref<DecisionCardStatus | ''>('PENDING')
const loading = ref(false)
const submitting = ref(false)
const errorMessage = ref('')

let wsHandle: WSConnectionHandle | null = null

async function loadDecisions() {
  loading.value = true
  errorMessage.value = ''
  try {
    cards.value = await listDecisions({ status: statusFilter.value })
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Failed to load decisions'
  } finally {
    loading.value = false
  }
}

async function handleRespond(cardId: string, payload: RespondDecisionPayload) {
  submitting.value = true
  errorMessage.value = ''
  try {
    await respondDecision(cardId, payload)
    await loadDecisions()
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Failed to submit decision'
  } finally {
    submitting.value = false
  }
}

onMounted(() => {
  void loadDecisions()
  wsHandle = connectWS(undefined, {
    onEvent: (ev) => {
      if (ev.topic === 'sofa.run.events') {
        void loadDecisions()
      }
    }
  })
})

onUnmounted(() => {
  wsHandle?.close()
  wsHandle = null
})
</script>

<template>
  <section class="decisions-view">
    <div class="card row" style="justify-content: space-between">
      <div class="row">
        <h2 style="margin: 0">Human Decisions &amp; Version-Bound Approvals</h2>
        <select v-model="statusFilter" @change="loadDecisions">
          <option value="PENDING">PENDING</option>
          <option value="ANSWERED">ANSWERED</option>
          <option value="INVALIDATED">INVALIDATED</option>
          <option value="">All</option>
        </select>
      </div>
      <button type="button" @click="loadDecisions" :disabled="loading">Refresh</button>
    </div>

    <p v-if="errorMessage" class="card error-banner">{{ errorMessage }}</p>

    <div v-if="cards.length === 0 && !loading" class="card muted">
      No matching decision cards.
    </div>

    <DecisionCardItem
      v-for="card in cards"
      :key="card.card_id"
      :card="card"
      :submitting="submitting"
      @respond="handleRespond"
    />
  </section>
</template>

<style scoped>
.decisions-view {
  display: flex;
  flex-direction: column;
  gap: 1rem;
}

.error-banner {
  border-color: var(--danger);
  color: #fca5a5;
}
</style>
