<script setup lang="ts">
import { ref } from 'vue'
import { clearApiToken, getApiToken, setApiToken } from './api/client'
import { currentRoute, navigateTo } from './router'
import DecisionsView from './views/DecisionsView.vue'
import RunDetailView from './views/RunDetailView.vue'
import RunsView from './views/RunsView.vue'
import SettingsView from './views/SettingsView.vue'

const tokenInput = ref(getApiToken())
const tokenSaved = ref(Boolean(getApiToken()))
const showTokenDialog = ref(false)

function saveToken() {
  setApiToken(tokenInput.value)
  tokenSaved.value = Boolean(getApiToken())
  showTokenDialog.value = false
}

function removeToken() {
  clearApiToken()
  tokenInput.value = ''
  tokenSaved.value = false
  showTokenDialog.value = false
}
</script>

<template>
  <div class="shell">
    <header class="topbar">
      <div class="row">
        <strong class="brand mono">sofa</strong>
        <nav class="row">
          <button
            type="button"
            :class="{ primary: currentRoute.name === 'runs' || currentRoute.name === 'run-detail' }"
            @click="navigateTo('/runs')"
          >
            Runs
          </button>
          <button
            type="button"
            :class="{ primary: currentRoute.name === 'decisions' }"
            @click="navigateTo('/decisions')"
          >
            Decisions
          </button>
          <button
            type="button"
            :class="{ primary: currentRoute.name === 'settings' }"
            @click="navigateTo('/settings')"
          >
            Settings
          </button>
        </nav>
      </div>

      <div class="row">
        <span class="badge">{{ tokenSaved ? 'Bearer Token (sessionStorage)' : 'Dev / No Token' }}</span>
        <button type="button" @click="showTokenDialog = !showTokenDialog">
          {{ showTokenDialog ? 'Close Auth' : 'API Token' }}
        </button>
      </div>
    </header>

    <div v-if="showTokenDialog" class="card token-dialog">
      <div class="row" style="justify-content: space-between">
        <div>
          <strong>Session Bearer Token (<code>SOFA_API_TOKEN</code>)</strong>
          <div class="muted">Stored strictly in <code>window.sessionStorage</code> for this browser tab.</div>
        </div>
        <form class="row" @submit.prevent="saveToken">
          <input
            v-model="tokenInput"
            type="password"
            placeholder="Enter SOFA_API_TOKEN"
            autocomplete="off"
          />
          <button type="submit" class="primary">Save Token</button>
          <button type="button" class="danger" @click="removeToken">Clear</button>
        </form>
      </div>
    </div>

    <main class="content">
      <RunDetailView
        v-if="currentRoute.name === 'run-detail' && currentRoute.runId"
        :run-id="currentRoute.runId"
        @back="navigateTo('/runs')"
        @deleted="navigateTo('/runs')"
      />
      <DecisionsView v-else-if="currentRoute.name === 'decisions'" />
      <SettingsView v-else-if="currentRoute.name === 'settings'" />
      <RunsView v-else />
    </main>
  </div>
</template>

<style scoped>
.shell {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
}

.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 1rem;
  flex-wrap: wrap;
  padding: 0.75rem 1.25rem;
  background: var(--bg-1);
  border-bottom: 1px solid var(--border);
}

.brand {
  font-size: 1.15rem;
  color: var(--accent);
  margin-right: 0.5rem;
}

.token-dialog {
  margin: 1rem 1.25rem 0;
  border-color: var(--accent);
}

.content {
  padding: 1.25rem;
  max-width: 1280px;
  width: 100%;
  margin: 0 auto;
}
</style>
