<script setup lang="ts">
import { ref } from 'vue'
import type { DecisionCard, PublishedArtifact, RespondDecisionPayload } from '../api/types'
import ArtifactSandboxPreview from './ArtifactSandboxPreview.vue'

const props = defineProps<{
  card: DecisionCard
  submitting?: boolean
}>()

const emit = defineEmits<{
  (e: 'respond', cardId: string, payload: RespondDecisionPayload): void
}>()

const selectedOption = ref(props.card.recommended || props.card.options?.[0] || '')
const answerText = ref('')
const returnFallback = ref('')
const previewArtifact = ref<PublishedArtifact | null>(null)

function isHtmlArtifact(art: PublishedArtifact): boolean {
  const lower = (art.rel_path || art.name || '').toLowerCase()
  return lower.endsWith('.html') || lower.endsWith('.htm') || art.type === 'directory'
}

function submitChoice() {
  emit('respond', props.card.card_id, {
    run_id: props.card.run_id,
    node_addr: props.card.node_addr,
    selected_option: selectedOption.value || undefined,
    answer_text: answerText.value
  })
}

function submitApprove() {
  emit('respond', props.card.card_id, {
    run_id: props.card.run_id,
    node_addr: props.card.node_addr,
    approved: true,
    commit_sha: props.card.commit_sha ?? '',
    artifact_ids: props.card.artifact_ids ?? [],
    answer_text: answerText.value
  })
}

function submitRejectRework() {
  emit('respond', props.card.card_id, {
    run_id: props.card.run_id,
    node_addr: props.card.node_addr,
    approved: false,
    commit_sha: props.card.commit_sha ?? '',
    artifact_ids: props.card.artifact_ids ?? [],
    answer_text: answerText.value
  })
}

function submitReturnUpstream() {
  emit('respond', props.card.card_id, {
    run_id: props.card.run_id,
    node_addr: props.card.node_addr,
    approved: false,
    returned_upstream: true,
    return_fallback: returnFallback.value.trim(),
    commit_sha: props.card.commit_sha ?? '',
    artifact_ids: props.card.artifact_ids ?? [],
    answer_text: answerText.value
  })
}
</script>

<template>
  <article class="card decision-card" :data-decision-id="card.card_id">
    <div class="row" style="justify-content: space-between">
      <div class="row">
        <span class="badge">{{ card.kind }}</span>
        <span class="badge" :class="`status-${card.status === 'PENDING' ? 'WAITING_HUMAN' : 'SUCCEEDED'}`">
          {{ card.status }}
        </span>
        <span class="mono">{{ card.run_id }} / {{ card.node_addr }}</span>
        <span class="badge mono">e{{ card.epoch }}</span>
      </div>
      <span class="muted mono">{{ card.card_id }}</span>
    </div>

    <p v-if="card.question" class="question-text">{{ card.question }}</p>
    <p v-if="card.pause_reason" class="question-text">{{ card.pause_reason }}</p>
    <p v-if="card.context" class="muted">{{ card.context }}</p>

    <div v-if="card.kind === 'human_approval'" class="fingerprint-box">
      <div><strong>Version-Bound Approval Fingerprint</strong></div>
      <div class="mono">
        commit_sha: <code>{{ card.commit_sha || '(none)' }}</code>
      </div>
      <div class="mono">
        artifact_ids: <code>{{ (card.artifact_ids ?? []).join(', ') || '(none)' }}</code>
      </div>
    </div>

    <div v-if="card.artifacts?.length" class="row">
      <button
        v-for="art in card.artifacts"
        :key="art.id"
        type="button"
        @click="previewArtifact = isHtmlArtifact(art) ? art : null"
      >
        Preview {{ art.rel_path || art.name }}
      </button>
    </div>

    <ArtifactSandboxPreview
      v-if="previewArtifact"
      :run-id="card.run_id"
      :artifact-id="previewArtifact.id"
      :title="previewArtifact.rel_path || previewArtifact.name"
    />

    <template v-if="card.status === 'PENDING'">
      <div v-if="card.options?.length" class="options-list">
        <label v-for="opt in card.options" :key="opt" class="row option-item">
          <input v-model="selectedOption" type="radio" :name="`opt-${card.card_id}`" :value="opt" />
          <span>{{ opt }}</span>
          <span v-if="opt === card.recommended" class="badge">recommended</span>
        </label>
      </div>

      <textarea
        v-model="answerText"
        rows="2"
        placeholder="Reply, clarification, or approval/rejection note..."
      />

      <div v-if="card.kind === 'human_approval'" class="approval-actions">
        <div class="row">
          <button
            type="button"
            class="primary"
            :data-testid="`approve-${card.card_id}`"
            :disabled="submitting"
            @click="submitApprove"
          >
            Approve &amp; Publish
          </button>
          <button
            type="button"
            class="danger"
            :disabled="submitting || !answerText.trim()"
            @click="submitRejectRework"
          >
            Reject for Rework
          </button>
        </div>

        <div class="row fallback-row">
          <input
            v-model="returnFallback"
            type="text"
            :data-testid="`fallback-input-${card.card_id}`"
            placeholder="Upstream fallback name (optional)"
          />
          <button
            type="button"
            class="warn"
            :data-testid="`return-upstream-${card.card_id}`"
            :disabled="submitting"
            @click="submitReturnUpstream"
          >
            Return Upstream Fallback
          </button>
        </div>
      </div>

      <div v-else class="row">
        <button
          type="button"
          class="primary"
          :disabled="submitting || (!selectedOption && !answerText.trim())"
          @click="submitChoice"
        >
          Submit Decision
        </button>
      </div>
    </template>
  </article>
</template>

<style scoped>
.decision-card {
  display: flex;
  flex-direction: column;
  gap: 0.7rem;
}

.question-text {
  margin: 0;
  font-weight: 600;
}

.fingerprint-box {
  background: #020617;
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 0.65rem 0.85rem;
  font-size: 0.85rem;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
}

.options-list {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
}

.approval-actions {
  display: flex;
  flex-direction: column;
  gap: 0.55rem;
}

.fallback-row {
  border-top: 1px dashed var(--border);
  padding-top: 0.55rem;
}
</style>
