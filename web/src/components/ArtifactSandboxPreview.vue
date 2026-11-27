<script setup lang="ts">
import { ref, watch } from 'vue'
import { mintPreviewUrl } from '../api/client'

const props = defineProps<{
  runId: string
  artifactId: string
  title?: string
}>()

const previewUrl = ref('')
const loading = ref(false)
const errorMessage = ref('')

async function loadPreview() {
  if (!props.runId || !props.artifactId) {
    previewUrl.value = ''
    return
  }
  loading.value = true
  errorMessage.value = ''
  try {
    previewUrl.value = await mintPreviewUrl(props.runId, props.artifactId)
  } catch (err) {
    previewUrl.value = ''
    errorMessage.value = err instanceof Error ? err.message : 'Failed to mint preview ticket'
  } finally {
    loading.value = false
  }
}

watch(() => [props.runId, props.artifactId], loadPreview, { immediate: true })
</script>

<template>
  <div class="sandbox-preview">
    <div class="sandbox-header row">
      <span class="mono">{{ title || artifactId }}</span>
      <span class="badge">sandbox: null-origin</span>
      <button type="button" @click="loadPreview" :disabled="loading">Refresh Ticket</button>
    </div>
    <p v-if="loading" class="muted">Minting scoped preview ticket...</p>
    <p v-else-if="errorMessage" class="error-text">{{ errorMessage }}</p>
    <iframe
      v-if="previewUrl"
      class="sandbox-frame"
      sandbox="allow-scripts"
      referrerpolicy="no-referrer"
      :src="previewUrl"
      :title="title || 'Untrusted Artifact Preview'"
    />
  </div>
</template>

<style scoped>
.sandbox-preview {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}

.sandbox-header {
  justify-content: space-between;
}

.sandbox-frame {
  width: 100%;
  min-height: 360px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: #ffffff;
}

.error-text {
  color: #fca5a5;
  margin: 0;
}
</style>
