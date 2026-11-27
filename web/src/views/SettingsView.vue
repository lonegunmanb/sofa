<script setup lang="ts">
import { onMounted, ref } from 'vue'
import {
  deleteEnv,
  deleteSecret,
  listEnvs,
  listSecrets,
  upsertEnv,
  upsertSecret
} from '../api/client'
import type { ConfigScope, EnvEntry, SecretMeta } from '../api/types'

const scope = ref<ConfigScope>('global')
const namespace = ref('default')

const envs = ref<EnvEntry[]>([])
const secrets = ref<SecretMeta[]>([])
const loading = ref(false)
const errorMessage = ref('')
const statusMessage = ref('')

const envName = ref('')
const envValue = ref('')

const secretName = ref('')
const secretValue = ref('')

async function loadConfig() {
  loading.value = true
  errorMessage.value = ''
  try {
    const ns = scope.value === 'namespace' ? namespace.value : undefined
    const [envList, secretList] = await Promise.all([
      listEnvs(scope.value, ns),
      listSecrets(scope.value, ns)
    ])
    envs.value = envList
    secrets.value = secretList
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Failed to load configuration'
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void loadConfig()
})

async function handleSaveEnv() {
  if (!envName.value.trim()) return
  errorMessage.value = ''
  statusMessage.value = ''
  try {
    await upsertEnv(
      scope.value,
      scope.value === 'namespace' ? namespace.value : undefined,
      envName.value.trim(),
      envValue.value
    )
    envName.value = ''
    envValue.value = ''
    statusMessage.value = 'Environment variable saved.'
    await loadConfig()
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Failed to save env'
  }
}

async function handleDeleteEnv(name: string) {
  errorMessage.value = ''
  try {
    await deleteEnv(scope.value, scope.value === 'namespace' ? namespace.value : undefined, name)
    await loadConfig()
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Failed to delete env'
  }
}

async function handleSaveSecret() {
  if (!secretName.value.trim() || !secretValue.value) return
  errorMessage.value = ''
  statusMessage.value = ''
  try {
    await upsertSecret(
      scope.value,
      scope.value === 'namespace' ? namespace.value : undefined,
      secretName.value.trim(),
      secretValue.value
    )
    secretName.value = ''
    secretValue.value = ''
    statusMessage.value = 'Write-only secret stored.'
    await loadConfig()
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Failed to save secret'
  }
}

async function handleDeleteSecret(name: string) {
  errorMessage.value = ''
  try {
    await deleteSecret(scope.value, scope.value === 'namespace' ? namespace.value : undefined, name)
    await loadConfig()
  } catch (err) {
    errorMessage.value = err instanceof Error ? err.message : 'Failed to delete secret'
  }
}

function prepareRotateSecret(name: string) {
  secretName.value = name
  secretValue.value = ''
}
</script>

<template>
  <section class="settings-view">
    <div class="card row" style="justify-content: space-between">
      <div class="row">
        <h2 style="margin: 0">Managed Config &amp; Write-Only Secrets</h2>
        <select v-model="scope" @change="loadConfig">
          <option value="global">Global</option>
          <option value="namespace">Namespace</option>
        </select>
        <input
          v-if="scope === 'namespace'"
          v-model="namespace"
          type="text"
          placeholder="Namespace (default)"
          @keyup.enter="loadConfig"
        />
      </div>
      <button type="button" @click="loadConfig" :disabled="loading">Refresh</button>
    </div>

    <p v-if="errorMessage" class="card error-banner">{{ errorMessage }}</p>
    <p v-if="statusMessage" class="card ok-banner">{{ statusMessage }}</p>

    <div class="card">
      <h3 style="margin-top: 0">Environment Variables (Plaintext)</h3>
      <form class="row" style="margin-bottom: 0.85rem" @submit.prevent="handleSaveEnv">
        <input v-model="envName" type="text" placeholder="KEY_NAME" />
        <input v-model="envValue" type="text" placeholder="Value" style="flex: 1" />
        <button type="submit" class="primary" :disabled="!envName.trim()">Set Env</button>
      </form>

      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Value</th>
            <th>Scope</th>
            <th>Updated</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="envs.length === 0">
            <td colspan="5" class="muted">No environment variables in this scope.</td>
          </tr>
          <tr v-for="e in envs" :key="e.name">
            <td class="mono">{{ e.name }}</td>
            <td class="mono">{{ e.value }}</td>
            <td><span class="badge">{{ e.namespace ? `${e.scope}:${e.namespace}` : e.scope }}</span></td>
            <td class="mono muted">{{ e.updated_at }}</td>
            <td>
              <button type="button" class="danger" @click="handleDeleteEnv(e.name)">Delete</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div class="card">
      <h3 style="margin-top: 0">Secrets (Strictly Write-Only)</h3>
      <p class="muted" style="margin-top: 0">
        Secret plaintext is encrypted at rest and never returned by any read or list endpoint.
      </p>
      <form class="row" style="margin-bottom: 0.85rem" @submit.prevent="handleSaveSecret">
        <input v-model="secretName" type="text" placeholder="SECRET_NAME" />
        <input
          v-model="secretValue"
          type="password"
          data-testid="secret-value-input"
          autocomplete="new-password"
          placeholder="Secret value (write-only)"
          style="flex: 1"
        />
        <button type="submit" class="primary" :disabled="!secretName.trim() || !secretValue">
          Store / Rotate Secret
        </button>
      </form>

      <table data-testid="secrets-table">
        <thead>
          <tr>
            <th>Name</th>
            <th>Mode</th>
            <th>Scope</th>
            <th>Updated</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="secrets.length === 0">
            <td colspan="5" class="muted">No secrets in this scope.</td>
          </tr>
          <tr v-for="s in secrets" :key="s.name">
            <td class="mono">{{ s.name }}</td>
            <td><span class="badge">write-only</span></td>
            <td><span class="badge">{{ s.namespace ? `${s.scope}:${s.namespace}` : s.scope }}</span></td>
            <td class="mono muted">{{ s.updated_at }}</td>
            <td>
              <div class="row">
                <button type="button" @click="prepareRotateSecret(s.name)">Rotate</button>
                <button type="button" class="danger" @click="handleDeleteSecret(s.name)">Delete</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<style scoped>
.settings-view {
  display: flex;
  flex-direction: column;
  gap: 1rem;
}

.error-banner {
  border-color: var(--danger);
  color: #fca5a5;
}

.ok-banner {
  border-color: var(--ok);
  color: #86efac;
}
</style>
