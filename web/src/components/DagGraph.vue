<script setup lang="ts">
import { computed } from 'vue'
import type { NodeRunSummary } from '../api/types'

const props = defineProps<{
  nodes: NodeRunSummary[]
  selectedNodeAddr?: string
}>()

const emit = defineEmits<{
  (e: 'select', nodeAddr: string): void
}>()

interface DagNodeView {
  node: NodeRunSummary
  modulePrefix: string
  shortName: string
  dynamicChildren: NodeRunSummary[]
  deps: string[]
}

function extractModulePrefix(node: NodeRunSummary): string {
  if (node.module_path) {
    return node.module_path
  }
  const addr = node.canonical_address || node.address
  const parts = addr.split('.')
  const modParts: string[] = []
  for (let i = 0; i + 1 < parts.length; i += 2) {
    if (parts[i] === 'module') {
      modParts.push(`module.${parts[i + 1]}`)
    } else {
      break
    }
  }
  return modParts.join('.')
}

function isDynamicInstance(node: NodeRunSummary): boolean {
  if (node.parent_dynamic_addr) return true
  const addr = node.canonical_address || node.address
  return addr.includes('[') && addr.endsWith(']')
}

function parentDynamicKey(addr: string): string {
  const idx = addr.indexOf('[')
  return idx > 0 ? addr.slice(0, idx) : ''
}

const layers = computed(() => {
  const all = props.nodes ?? []
  const byAddr = new Map<string, NodeRunSummary>()
  for (const n of all) {
    byAddr.set(n.canonical_address || n.address, n)
  }

  const childrenMap = new Map<string, NodeRunSummary[]>()
  const topNodes: NodeRunSummary[] = []

  for (const n of all) {
    const addr = n.canonical_address || n.address
    if (isDynamicInstance(n)) {
      const parentAddr = n.parent_dynamic_addr || parentDynamicKey(addr)
      if (parentAddr && byAddr.has(parentAddr)) {
        const list = childrenMap.get(parentAddr) ?? []
        list.push(n)
        childrenMap.set(parentAddr, list)
        continue
      }
    }
    topNodes.push(n)
  }

  // Compute topological layer for each top-level node
  const depthMemo = new Map<string, number>()
  const visiting = new Set<string>()

  function getDepth(addr: string): number {
    if (depthMemo.has(addr)) return depthMemo.get(addr)!
    if (visiting.has(addr)) return 0
    visiting.add(addr)
    const node = byAddr.get(addr)
    let depth = 0
    if (node?.depends_on && node.depends_on.length > 0) {
      for (const dep of node.depends_on) {
        if (byAddr.has(dep)) {
          depth = Math.max(depth, getDepth(dep) + 1)
        }
      }
    } else if (node?.kind === 'merge') {
      depth = 2
    } else if (node?.kind === 'dynamic_worker') {
      depth = 1
    }
    visiting.delete(addr)
    depthMemo.set(addr, depth)
    return depth
  }

  const layerBuckets = new Map<number, DagNodeView[]>()
  for (const n of topNodes) {
    const addr = n.canonical_address || n.address
    const d = getDepth(addr)
    const modPrefix = extractModulePrefix(n)
    const shortName = modPrefix && addr.startsWith(modPrefix + '.') ? addr.slice(modPrefix.length + 1) : addr
    const view: DagNodeView = {
      node: n,
      modulePrefix: modPrefix,
      shortName,
      dynamicChildren: childrenMap.get(addr) ?? [],
      deps: n.depends_on ?? []
    }
    const bucket = layerBuckets.get(d) ?? []
    bucket.push(view)
    layerBuckets.set(d, bucket)
  }

  return Array.from(layerBuckets.entries())
    .sort((a, b) => a[0] - b[0])
    .map(([layerIndex, items]) => ({
      layerIndex,
      items
    }))
})
</script>

<template>
  <div class="dag-container">
    <div v-if="layers.length === 0" class="muted">No DAG nodes reported yet.</div>
    <div class="dag-layers" v-else>
      <div v-for="layer in layers" :key="layer.layerIndex" class="dag-layer">
        <div class="layer-title muted">Stage {{ layer.layerIndex + 1 }}</div>
        <div class="layer-nodes">
          <div
            v-for="item in layer.items"
            :key="item.node.canonical_address"
            class="dag-node-group"
          >
            <button
              type="button"
              class="dag-node-card"
              :class="{ selected: selectedNodeAddr === item.node.canonical_address }"
              :data-node-addr="item.node.canonical_address"
              @click="emit('select', item.node.canonical_address)"
            >
              <div class="node-head row">
                <span class="badge kind-badge">{{ item.node.kind }}</span>
                <span v-if="item.modulePrefix" class="badge mod-badge mono">{{ item.modulePrefix }}</span>
                <span class="badge" :class="`status-${item.node.status}`">{{ item.node.status }}</span>
                <span class="badge mono">e{{ item.node.current_epoch }}</span>
              </div>
              <div class="node-title mono">{{ item.shortName }}</div>
              <div v-if="item.deps.length > 0" class="node-deps muted mono">
                ← {{ item.deps.join(', ') }}
              </div>
              <div v-if="item.node.work_branch" class="node-meta muted mono">
                {{ item.node.work_branch }}
              </div>
              <div v-if="item.node.dynamic_keys?.length" class="node-meta muted">
                {{ item.node.dynamic_keys.length }} expanded instances
              </div>
            </button>

            <div v-if="item.dynamicChildren.length > 0" class="dynamic-children">
              <button
                v-for="child in item.dynamicChildren"
                :key="child.canonical_address"
                type="button"
                class="dag-child-card"
                :class="{ selected: selectedNodeAddr === child.canonical_address }"
                :data-node-addr="child.canonical_address"
                @click="emit('select', child.canonical_address)"
              >
                <span class="mono child-addr">{{ child.canonical_address }}</span>
                <span class="badge" :class="`status-${child.status}`">{{ child.status }}</span>
                <span class="badge mono">e{{ child.current_epoch }}</span>
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.dag-layers {
  display: flex;
  gap: 1rem;
  overflow-x: auto;
  padding-bottom: 0.5rem;
}

.dag-layer {
  min-width: 260px;
  flex: 1 0 260px;
  display: flex;
  flex-direction: column;
  gap: 0.65rem;
  background: rgba(15, 23, 42, 0.5);
  border: 1px solid var(--border);
  border-radius: 10px;
  padding: 0.75rem;
}

.layer-title {
  font-size: 0.78rem;
  text-transform: uppercase;
  letter-spacing: 0.05em;
}

.layer-nodes {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}

.dag-node-group {
  display: flex;
  flex-direction: column;
  gap: 0.4rem;
}

.dag-node-card {
  width: 100%;
  text-align: left;
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  padding: 0.75rem;
  background: var(--bg-1);
}

.dag-node-card.selected,
.dag-child-card.selected {
  border-color: var(--accent);
  box-shadow: 0 0 0 1px var(--accent);
}

.node-title {
  font-weight: 600;
  word-break: break-all;
}

.node-deps,
.node-meta {
  font-size: 0.78rem;
  word-break: break-all;
}

.mod-badge {
  color: #7dd3fc;
}

.dynamic-children {
  margin-left: 0.85rem;
  padding-left: 0.65rem;
  border-left: 2px dashed var(--border);
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
}

.dag-child-card {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.4rem;
  padding: 0.45rem 0.6rem;
  font-size: 0.82rem;
  background: #0f172a;
}

.child-addr {
  overflow: hidden;
  text-overflow: ellipsis;
}
</style>
